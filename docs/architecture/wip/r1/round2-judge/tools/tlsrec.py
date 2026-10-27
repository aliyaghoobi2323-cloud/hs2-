#!/usr/bin/env python3
"""tlsrec.py: TLS record lengths on the wire, from a pcap (the R1 judge, T14 i).

  tlsrec.py extract PCAP OUT.json     every TLS record of every TCP stream on
                                      port 2096: direction, time, type, length
  tlsrec.py compare A.json B.json ... record-length distributions against the
                                      first (base): KS and chi-square tests,
                                      a 16-record flow classifier (AUC), the
                                      inter-record gaps; and real_sizes files
                                      for obfs/dpi_eval2.py

Standard library only (the host has no numpy/scipy). TCP streams are
reassembled by sequence number; a stream captured from its SYN is walked from
its first byte, otherwise from the first offset where three records chain.
"""
import json
import math
import random
import statistics as st
import struct
import sys

PORT = 2096


def pcap_packets(path):
    with open(path, "rb") as f:
        gh = f.read(24)
        magic = struct.unpack("<I", gh[:4])[0]
        if magic in (0xa1b2c3d4, 0xa1b23c4d):
            en = "<"
        else:
            en = ">"
        nano = magic in (0xa1b23c4d, 0x4d3cb2a1)
        lt = struct.unpack(en + "I", gh[20:24])[0]
        while True:
            h = f.read(16)
            if len(h) < 16:
                return
            ts, tu, cl, ol = struct.unpack(en + "IIII", h)
            data = f.read(cl)
            t = ts + tu / (1e9 if nano else 1e6)
            yield lt, t, data


def parse(lt, data):
    if lt == 1:  # ethernet
        off = 14
        et = struct.unpack(">H", data[12:14])[0]
        if et == 0x8100:
            et = struct.unpack(">H", data[16:18])[0]
            off = 18
    elif lt == 113:  # linux cooked
        et = struct.unpack(">H", data[14:16])[0]
        off = 16
    elif lt == 276:  # linux cooked v2
        et = struct.unpack(">H", data[0:2])[0]
        off = 20
    else:
        return None
    if et != 0x0800:
        return None
    ip = data[off:]
    ihl = (ip[0] & 15) * 4
    tot = struct.unpack(">H", ip[2:4])[0]
    if ip[9] != 6:
        return None
    src, dst = ip[12:16], ip[16:20]
    tcp = ip[ihl:tot]
    sp, dp, seq = struct.unpack(">HHI", tcp[:8])
    doff = (tcp[12] >> 4) * 4
    flags = tcp[13]
    return (src, sp, dst, dp), seq, flags, tcp[doff:]


def extract(pcap, out):
    streams = {}
    for lt, t, data in pcap_packets(pcap):
        p = parse(lt, data)
        if not p:
            continue
        key, seq, flags, pl = p
        if key[1] != PORT and key[3] != PORT:
            continue
        s = streams.setdefault(key, {"isn": None, "segs": {}})
        if flags & 0x02:  # SYN
            s["isn"] = seq
        if pl:
            if seq not in s["segs"] or len(s["segs"][seq][1]) < len(pl):
                s["segs"][seq] = (t, pl)
    res = []
    for key, s in streams.items():
        if not s["segs"]:
            continue
        # direction: from the edge's listener (port 2096) = edge -> exit (upload)
        dirn = "edge>exit" if key[1] == PORT else "exit>edge"
        segs = sorted(s["segs"].items(), key=lambda x: ((x[0] - (s["isn"] or 0)) & 0xffffffff))
        base = ((s["isn"] + 1) & 0xffffffff) if s["isn"] is not None else segs[0][0]
        # contiguous byte runs: (start offset, bytes, [(offset, time)])
        runs = []
        cur, buf, times = None, bytearray(), []
        for seq, (t, pl) in segs:
            o = (seq - base) & 0xffffffff
            if o > 1 << 31:
                continue
            if cur is None:
                cur, buf, times = o, bytearray(pl), [(o, t)]
                continue
            end = cur + len(buf)
            if o > end:
                runs.append((cur, bytes(buf), times))
                cur, buf, times = o, bytearray(pl), [(o, t)]
            elif o + len(pl) > end:
                buf += pl[end - o:]
                times.append((end, t))
        if cur is not None:
            runs.append((cur, bytes(buf), times))
        for k, (start, b, times) in enumerate(runs):
            synced = (k == 0 and start == 0 and s["isn"] is not None)
            i = 0 if synced else sync(b)
            if i is None:
                continue
            ti = 0
            while i + 5 <= len(b):
                typ, ver, ln = b[i], struct.unpack(">H", b[i + 1:i + 3])[0], struct.unpack(">H", b[i + 3:i + 5])[0]
                if typ not in (20, 21, 22, 23) or ver not in (0x0301, 0x0302, 0x0303) or ln > 18432:
                    i = sync(b, i + 1)
                    if i is None:
                        break
                    continue
                if i + 5 + ln > len(b):
                    break
                while ti + 1 < len(times) and times[ti + 1][0] - start <= i:
                    ti += 1
                res.append({"c": "%s:%d>%s:%d" % (".".join(map(str, key[0])), key[1], ".".join(map(str, key[2])), key[3]),
                            "d": dirn, "t": round(times[ti][1], 6), "type": typ, "len": ln})
                i += 5 + ln
    json.dump(res, open(out, "w"))
    print("%s: %d records in %d streams" % (out, len(res), len(streams)))


def sync(b, i=0):
    """the first offset from i where three TLS application records chain"""
    n = len(b)
    while i + 5 <= n:
        j = b.find(b"\x17\x03\x03", i)
        if j < 0 or j + 5 > n:
            return None
        k, ok = j, 0
        while ok < 3 and k + 5 <= n and b[k] == 23 and b[k + 1:k + 3] == b"\x03\x03":
            ln = struct.unpack(">H", b[k + 3:k + 5])[0]
            if ln > 18432:
                break
            k += 5 + ln
            ok += 1
        if ok >= 3 or (ok >= 1 and k == n):
            return j
        i = j + 1
    return None


# ------------------------------------------------------------------ statistics

def ks2(a, b):
    a, b = sorted(a), sorted(b)
    na, nb = len(a), len(b)
    i = j = 0
    d = 0.0
    while i < na and j < nb:
        x = min(a[i], b[j])
        while i < na and a[i] <= x:
            i += 1
        while j < nb and b[j] <= x:
            j += 1
        d = max(d, abs(i / na - j / nb))
    en = math.sqrt(na * nb / (na + nb))
    lam = (en + 0.12 + 0.11 / en) * d
    p = 0.0
    for k in range(1, 101):
        p += 2 * (-1) ** (k - 1) * math.exp(-2 * k * k * lam * lam)
    return d, max(0.0, min(1.0, p))


def gammq(a, x):
    """regularized upper incomplete gamma Q(a, x)"""
    if x <= 0:
        return 1.0
    gl = math.lgamma(a)
    if x < a + 1:
        ap, s, d = a, 1.0 / a, 1.0 / a
        for _ in range(1000):
            ap += 1
            d *= x / ap
            s += d
            if abs(d) < abs(s) * 1e-12:
                break
        return 1 - s * math.exp(-x + a * math.log(x) - gl)
    b = x + 1 - a
    c = 1 / 1e-300
    d = 1 / b
    h = d
    for i in range(1, 1000):
        an = -i * (i - a)
        b += 2
        d = an * d + b
        d = 1e-300 if abs(d) < 1e-300 else d
        c = b + an / c
        c = 1e-300 if abs(c) < 1e-300 else c
        d = 1 / d
        de = d * c
        h *= de
        if abs(de - 1) < 1e-12:
            break
    return math.exp(-x + a * math.log(x) - gl) * h


EDGES = [0, 64, 128, 256, 512, 768, 1024, 1280, 1400, 1450, 1500, 2048, 4096, 8192, 16384, 1 << 20]


def hist(v):
    h = [0] * (len(EDGES) - 1)
    for x in v:
        for k in range(len(EDGES) - 1):
            if EDGES[k] <= x < EDGES[k + 1]:
                h[k] += 1
                break
    return h


def chi2(a, b):
    ha, hb = hist(a), hist(b)
    na, nb = sum(ha), sum(hb)
    x, dof = 0.0, -1
    for p, q in zip(ha, hb):
        if p + q == 0:
            continue
        dof += 1
        ea = (p + q) * na / (na + nb)
        eb = (p + q) * nb / (na + nb)
        x += (p - ea) ** 2 / ea + (q - eb) ** 2 / eb
    return x, dof, gammq(dof / 2, x / 2) if dof > 0 else 1.0


def train(pos, neg, it=300, lr=0.02):
    allx = pos + neg
    mean = [st.mean(c) for c in zip(*allx)]
    sd = [st.pstdev(c) or 1 for c in zip(*allx)]
    X = [([(a - m) / s for a, m, s in zip(x, mean, sd)], 1) for x in pos] + \
        [([(a - m) / s for a, m, s in zip(x, mean, sd)], 0) for x in neg]
    w = [0.0] * len(pos[0])
    b = 0.0
    for _ in range(it):
        random.shuffle(X)
        for x, y in X:
            z = b + sum(wi * xi for wi, xi in zip(w, x))
            p = 1 / (1 + math.exp(-max(-30, min(30, z))))
            g = p - y
            for j in range(len(w)):
                w[j] -= lr * g * x[j]
            b -= lr * g
    return w, b, mean, sd


def auc(m, pos, neg):
    w, b, mean, sd = m

    def sc(x):
        xn = [(a - mm) / s for a, mm, s in zip(x, mean, sd)]
        return b + sum(wi * xi for wi, xi in zip(w, xn))
    ps, ns = [sc(x) for x in pos], [sc(x) for x in neg]
    win = 0.0
    for a in ps:
        for c in ns:
            win += 1 if a > c else 0.5 if a == c else 0
    return win / (len(ps) * len(ns))


def flows(recs, k=16, gaps=False):
    """consecutive application records of one stream, k at a time: features"""
    by = {}
    for r in recs:
        if r["type"] == 23:
            by.setdefault(r["c"], []).append(r)
    out = []
    for c, rs in by.items():
        rs.sort(key=lambda r: r["t"])
        for i in range(0, len(rs) - k + 1, k):
            s = [r["len"] for r in rs[i:i + k]]
            f = s[:6] + [st.mean(s), st.pstdev(s), len(set(s))]
            if gaps:
                g = [1000 * (rs[i + j + 1]["t"] - rs[i + j]["t"]) for j in range(k - 1)]
                f += [st.mean(g), st.pstdev(g)]
            out.append(f)
    return out


def compare(paths):
    random.seed(17)
    data = [(p, json.load(open(p))) for p in paths]
    base_p, base = data[0]
    print("| ضبط | جهت | رکورد کاربردی | میانه | p10 | p90 | ≥1400 B | KS D (p) | χ² (درجه، p) | AUC طبقه‌بند ۱۶ رکوردی | AUC با فاصله‌ها |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for p, recs in data:
        for d in ("exit>edge", "edge>exit"):
            v = [r["len"] for r in recs if r["type"] == 23 and r["d"] == d]
            bv = [r["len"] for r in base if r["type"] == 23 and r["d"] == d]
            if not v:
                continue
            q = sorted(v)
            row = [p.split("/")[-1], d, str(len(v)), "%d" % st.median(v), "%d" % q[len(q) // 10],
                   "%d" % q[9 * len(q) // 10], "%.1f%%" % (100 * sum(1 for x in v if x >= 1400) / len(v))]
            if p == base_p:
                row += ["-", "-", "-", "-"]
            else:
                D, pk = ks2(bv, v)
                x, dof, pc = chi2(bv, v)
                fa = flows([r for r in recs if r["d"] == d])
                fb = flows([r for r in base if r["d"] == d])
                n = min(len(fa), len(fb), 400)
                random.shuffle(fa)
                random.shuffle(fb)
                aucs = "-"
                aucg = "-"
                if n >= 40:
                    m = train(fa[:n // 2], fb[:n // 2])
                    aucs = "%.3f" % auc(m, fa[n // 2:n], fb[n // 2:n])
                    ga = flows([r for r in recs if r["d"] == d], gaps=True)
                    gb = flows([r for r in base if r["d"] == d], gaps=True)
                    random.shuffle(ga)
                    random.shuffle(gb)
                    m = train(ga[:n // 2], gb[:n // 2])
                    aucg = "%.3f" % auc(m, ga[n // 2:n], gb[n // 2:n])
                row += ["%.4f (%.3f)" % (D, pk), "%.1f (%d، %.3f)" % (x, dof, pc), aucs, aucg]
            print("| " + " | ".join(row) + " |")
        with open(p[:-5] + ".real_sizes.txt", "w") as f:
            for r in recs:
                if r["type"] == 23:
                    f.write("%d\n" % r["len"])


if __name__ == "__main__":
    if sys.argv[1] == "extract":
        extract(sys.argv[2], sys.argv[3])
    else:
        compare(sys.argv[2:])

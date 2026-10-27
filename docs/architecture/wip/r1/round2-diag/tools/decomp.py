#!/usr/bin/env python3
"""decomp.py RUNDIR [RATE_MBIT] [RTT_MS]: cut each probe's round trip into its
parts (R1 round 2, step 1; lab only, docs/architecture/wip/r1/round2-diagnosis.fa.md).

Inputs in RUNDIR (run.sh with TRACE=1 CAP=1 on r1-diag): trace/trace-*.txt
(the two hs2 processes' Q/W/R lines), probes.json (rigload's send times and
RTTs), e0.pcap(.gz) and x0.pcap(.gz) (the link's packet headers on the edge's
and the exit's veth). For each probe and each direction (up: edge -> exit,
down: exit -> edge), wall-clock ms:

  user    the user's socket to hs2 (up: rigload's send -> the edge's frame
          pushed to the scheduler; down: the exit's reader parsed the
          request -> the reply frame pushed: rigload server's echo, local)
  sched   pushed -> the writer took it (C2's wait)
  write   taken -> the TLS record carrying it went into the socket (the
          shaper's held tail, C3's gate)
  sock    into the socket -> its last byte's first transmission on the
          sender's veth (unsent bytes ahead, cwnd/pacing, the host qdisc)
  path    first transmission -> first arrival on the receiver's veth
          (propagation RTT/2, the bottleneck's queue, serialization; plus the
          retransmission's delay when that very segment was lost)
  hol     arrival -> every byte before it arrived (waiting behind a hole:
          head-of-line inside TCP)
  recv    all arrived -> the receiving hs2 parsed the frame (wakeup, TLS,
          smux)
  last    (down only) parsed by the edge -> rigload got it

and per probe: lost_own (its segment was sent more than once), queue
(path - RTT/2 - one MTU's serialization). Writes RUNDIR/decomp.json (per
probe rows and a summary) and prints the summary."""
import gzip, json, os, struct, sys, glob, bisect

def openp(path):
    if os.path.exists(path):
        return open(path, "rb")
    if os.path.exists(path + ".gz"):
        return gzip.open(path + ".gz", "rb")
    return None

def pcap_packets(path):
    """(ts, src, sport, dst, dport, seq, flags, plen) of each TCP packet"""
    f = openp(path)
    if f is None:
        return []
    data = f.read()
    f.close()
    if len(data) < 24:
        return []
    magic = struct.unpack_from("<I", data, 0)[0]
    if magic == 0xa1b2c3d4:
        e, nano = "<", False
    elif magic == 0xa1b23c4d:
        e, nano = "<", True
    else:
        raise SystemExit("pcap magic %x" % magic)
    lt = struct.unpack_from(e + "I", data, 20)[0]
    off = 24
    out = []
    n = len(data)
    hdr = struct.Struct(e + "IIII")
    div = 1e9 if nano else 1e6
    while off + 16 <= n:
        s, us, incl, orig = hdr.unpack_from(data, off)
        off += 16
        p = off
        off += incl
        if lt == 1:
            if data[p + 12] != 8 or data[p + 13] != 0:
                continue
            p += 14
        elif lt == 113:
            p += 16
        else:
            continue
        vihl = data[p]
        if vihl >> 4 != 4 or data[p + 9] != 6:
            continue
        ihl = (vihl & 15) * 4
        tot = (data[p + 2] << 8) | data[p + 3]
        src = data[p + 12:p + 16]
        dst = data[p + 16:p + 20]
        t = p + ihl
        sport, dport, seq = struct.unpack_from("!HHI", data, t)
        doff = (data[t + 12] >> 4) * 4
        flags = data[t + 13]
        plen = tot - ihl - doff
        out.append((s + us / div, bytes(src), sport, bytes(dst), dport, seq, flags, plen))
    return out

EXIT_IP = bytes([10, 202, 0, 1])
EDGE_IP = bytes([10, 201, 0, 1])

def streams(pk):
    """per connection (the exit's port) and direction: ISN and the data
    packets (ts, start, end) in relative byte offsets"""
    isn = {}
    for ts, src, sp, dst, dp, seq, fl, pl in pk:
        if fl & 0x02:  # SYN
            if src == EXIT_IP:
                isn[(sp, "dn")] = seq
            else:
                isn[(dp, "up")] = seq
    out = {}
    for ts, src, sp, dst, dp, seq, fl, pl in pk:
        if pl <= 0:
            continue
        if src == EXIT_IP and sp != 2096:
            key = (sp, "dn")
        elif src == EDGE_IP and sp == 2096:
            key = (dp, "up")
        else:
            continue
        i = isn.get(key)
        if i is None:
            continue
        a = (seq - i - 1) & 0xffffffff
        out.setdefault(key, []).append((ts, a, a + pl))
    return out

class Seg:
    """a direction's segments as seen on one veth, for lookups"""
    def __init__(self, segs):
        self.segs = segs  # time order

    def first_cover(self, targets):
        """target offset -> (first ts covering it, how many times covered)"""
        tg = sorted(set(targets))
        first, cnt = {}, {}
        for ts, a, b in self.segs:
            i = bisect.bisect_left(tg, a)
            while i < len(tg) and tg[i] < b:
                x = tg[i]
                if x not in first:
                    first[x] = ts
                cnt[x] = cnt.get(x, 0) + 1
                i += 1
        return first, cnt

    def contig(self, targets):
        """target offset -> first ts at which every byte up to it arrived"""
        tg = sorted(set(targets))
        res = {}
        nxt = 0
        ooo = []  # sorted disjoint [a, b)
        j = 0
        for ts, a, b in self.segs:
            if b <= nxt:
                continue
            if a <= nxt:
                nxt = b
                while ooo and ooo[0][0] <= nxt:
                    nxt = max(nxt, ooo.pop(0)[1])
            else:
                k = bisect.bisect_left(ooo, (a, b))
                ooo.insert(k, (a, b))
                # merge neighbours (keep it simple: merge all overlaps)
                m = []
                for x, y in ooo:
                    if m and x <= m[-1][1]:
                        m[-1] = (m[-1][0], max(m[-1][1], y))
                    else:
                        m.append((x, y))
                ooo = m
            while j < len(tg) and tg[j] < nxt:
                res[tg[j]] = ts
                j += 1
        return res

def read_traces(rd):
    roles = {}
    for p in glob.glob(os.path.join(rd, "trace", "trace-*.txt")):
        role = None
        L = {"Q": [], "W": [], "R": [], "T": [], "S": []}
        for line in open(p, errors="replace"):
            t = line.split()
            if not t:
                continue
            k = t[0]
            if k == "H":
                role = "edge" if "edge.json" in line else ("exit" if "exit.json" in line else None)
            elif k in L:
                L[k].append(t)
        if role:
            roles[role] = L
    return roles

def pct(v, q):
    if not v:
        return None
    v = sorted(v)
    return v[min(len(v) - 1, int(round(q * (len(v) - 1))))]

PARTS_UP = ["user", "sched", "write", "sock", "path", "hol", "recv"]
PARTS_DN = ["user", "sched", "write", "sock", "path", "hol", "recv", "last"]

def main():
    rd = sys.argv[1]
    rate = float(sys.argv[2]) if len(sys.argv) > 2 else None
    rtt = float(sys.argv[3]) if len(sys.argv) > 3 else 72.0
    meta = {}
    try:
        meta = json.load(open(os.path.join(rd, "meta.json")))
    except Exception:
        pass
    if rate is None:
        for tok in meta.get("cell", "").split():
            if tok.startswith("RATE="):
                rate = float(tok[5:])
            if tok.startswith("RTT="):
                rtt = float(tok[4:])
    ser = 1514 * 8 / (rate * 1e6) * 1e3 if rate else 0.0
    tr = read_traces(rd)
    pr = json.load(open(os.path.join(rd, "probes.json")))
    t0 = pr["t0_ns"] / 1e9
    users = [u for u in pr["users"] if u.get("probe") and u.get("path", "port") == "port"]
    if not users:
        raise SystemExit("no probe user")
    # probes: seq -> (send wall s, rtt ms or None)
    probes = {}
    u = users[0]
    pd = u["probe"]
    samp = {round(s, 6): m for s, m in zip(pd["samp"], pd["ms"])}
    for seq, s in enumerate(pd["send"]):
        probes[seq] = (t0 + s, samp.get(round(s, 6)))
    multi = len(users) > 1

    def frames(role, kind):
        """probe frames by seq: the line. A probe is 32 bytes, its sequence
        number then zeros; a frame may carry up to 4 of them (a reader that
        found two waiting writes them as one frame)"""
        out = {}
        for t in tr.get(role, {}).get(kind, []):
            n = int(t[3])
            if n % 32 or n > 128:
                continue
            key = t[4]
            if len(key) < n * 2:
                continue
            chunks = [key[i * 64:(i + 1) * 64] for i in range(n // 32)]
            if any(c[8:] != "0" * 56 for c in chunks):
                continue
            for c in chunks:
                out.setdefault(int(c[:8], 16), []).append(t)
        return out

    eQ, eW, eR = frames("edge", "Q"), frames("edge", "W"), frames("edge", "R")
    xQ, xW, xR = frames("exit", "Q"), frames("exit", "W"), frames("exit", "R")
    pk_e = pcap_packets(os.path.join(rd, "e0.pcap"))
    pk_x = pcap_packets(os.path.join(rd, "x0.pcap"))
    se, sx = streams(pk_e), streams(pk_x)
    # the links' TCP connections, by the exit's port
    conns = sorted(set(k[0] for k in list(se) + list(sx)))

    def wl(t):
        # W line fields after the key: t_write t_rec off lport rport srtt minrtt rttvar cwnd
        # unacked notsent outq total_retrans lost ca_state delivery pacing retrans_now sacked mss backoff
        f = t[5:]
        d = {"t_write": int(f[0]) / 1e9, "t_rec": int(f[1]) / 1e9, "off": int(f[2])}
        if len(f) >= 21:
            names = ["lport", "rport", "srtt_us", "minrtt_us", "rttvar_us", "cwnd", "unacked", "notsent", "outq",
                     "total_retrans", "lost", "ca_state", "delivery_Bps", "pacing_Bps", "retrans_now", "sacked", "mss", "backoff"]
            for n, v in zip(names, f[3:]):
                d[n] = int(v)
        return d

    # targets per (conn, dir): the last byte of the record with the probe
    want = {}
    rows = []
    for seq, (ts, ms) in sorted(probes.items()):
        r = {"seq": seq, "t_send": ts, "rtt": ms}
        for d, Q, W, R, sender in (("up", eQ, eW, xR, "edge"), ("dn", xQ, xW, eR, "exit")):
            q, w, rr = Q.get(seq), W.get(seq), R.get(seq)
            if not q or not w or not rr or len(q) > 1 or len(w) > 1 or len(rr) > 1:
                r[d] = None
                continue
            q, w, rr = q[0], w[0], rr[0]
            x = {"t_push": int(q[5]) / 1e9, "t_take": int(q[6]) / 1e9, "lane": q[7], "t_R": int(rr[5]) / 1e9}
            x.update(wl(w))
            if x["off"] > 0 and "lport" in x:
                conn = x["lport"] if sender == "exit" else x["rport"]
                x["conn"] = conn
                # the TLS record's last byte: off - 1; the exit opened the
                # link (active open) and its bytes_acked counts the SYN
                # (measured: off - 1 is a segment end on the exit's side,
                # off on the edge's)
                x["S"] = x["off"] - 1 - (1 if sender == "exit" else 0)
                want.setdefault((conn, d), []).append(x["S"])
            r[d] = x
        rows.append(r)
    # the captures
    look = {}
    for (conn, d), tg in want.items():
        snd = sx if d == "dn" else se
        rcv = se if d == "dn" else sx
        if (conn, d) not in snd or (conn, d) not in rcv:
            continue
        S1, S2 = Seg(snd[(conn, d)]), Seg(rcv[(conn, d)])
        tx, ntx = S1.first_cover(tg)
        arr, _ = S2.first_cover(tg)
        cg = S2.contig(tg)
        look[(conn, d)] = (tx, ntx, arr, cg)
    out = []
    for r in rows:
        o = {"seq": r["seq"], "rtt": r["rtt"]}
        ok = r["rtt"] is not None
        for d in ("up", "dn"):
            x = r[d]
            if x is None or "S" not in x or (x["conn"], d) not in look:
                ok = False
                continue
            tx, ntx, arr, cg = look[(x["conn"], d)]
            S = x["S"]
            if S not in tx or S not in arr or S not in cg:
                ok = False
                continue
            p = {}
            p["sched"] = (x["t_take"] - x["t_push"]) * 1e3
            p["write"] = (x["t_rec"] - x["t_take"]) * 1e3
            p["sock"] = (tx[S] - x["t_rec"]) * 1e3
            p["path"] = (arr[S] - tx[S]) * 1e3
            p["hol"] = (cg[S] - arr[S]) * 1e3
            p["recv"] = (x["t_R"] - cg[S]) * 1e3
            p["queue"] = p["path"] - rtt / 2 - ser
            p["ntx"] = ntx.get(S, 0)
            p["srtt"] = x.get("srtt_us", 0) / 1e3
            p["minrtt"] = x.get("minrtt_us", 0) / 1e3
            p["notsent"] = x.get("notsent")
            p["outq"] = x.get("outq")
            p["cwnd"] = x.get("cwnd")
            p["unacked"] = x.get("unacked")
            p["deliv_mbit"] = x.get("delivery_Bps", 0) * 8 / 1e6
            p["lane"] = x["lane"]
            p["conn"] = x["conn"]
            o[d] = p
        if ok:
            up, dn = r["up"], r["dn"]
            o["up"]["user"] = (up["t_push"] - r["t_send"]) * 1e3
            o["dn"]["user"] = (dn["t_push"] - up["t_R"]) * 1e3
            o["dn"]["last"] = (r["t_send"] + r["rtt"] / 1e3 - dn["t_R"]) * 1e3
            o["sum"] = sum(o["up"][k] for k in PARTS_UP) + sum(o["dn"][k] for k in PARTS_DN)
        o["ok"] = ok
        out.append(o)
    good = [o for o in out if o["ok"]]
    summ = {"probes": len(out), "decomposed": len(good), "answered": sum(1 for o in out if o["rtt"] is not None),
            "multi_probe_users": multi, "conns": conns, "rate": rate, "rtt": rtt}
    if good:
        rt = sorted(o["rtt"] for o in good)
        summ["rtt_p1"], summ["rtt_p50"], summ["rtt_p99"] = pct(rt, .01), pct(rt, .5), pct(rt, .99)
        summ["sum_err_ms_p50"] = pct([abs(o["sum"] - o["rtt"]) for o in good], .5)
        comp = {}
        for d, parts in (("up", PARTS_UP + ["queue"]), ("dn", PARTS_DN + ["queue"])):
            for k in parts:
                v = [o[d][k] for o in good]
                comp[d + "." + k] = {"p50": pct(v, .5), "p90": pct(v, .9), "p99": pct(v, .99), "mean": sum(v) / len(v)}
        summ["parts"] = comp
        # the slowest probes: mean of each part over the slowest 5% and 1%
        srt = sorted(good, key=lambda o: o["rtt"])
        for name, frac in (("slow5", .05), ("slow1", .01), ("all", 1.0)):
            k = max(1, int(round(frac * len(srt))))
            sl = srt[-k:]
            m = {"n": k, "rtt": sum(o["rtt"] for o in sl) / k}
            for d, parts in (("up", PARTS_UP), ("dn", PARTS_DN)):
                for p in parts:
                    m[d + "." + p] = sum(o[d][p] for o in sl) / k
                m[d + ".queue"] = sum(o[d]["queue"] for o in sl) / k
                m[d + ".lost_own"] = sum(1 for o in sl if o[d]["ntx"] > 1) / k
                m[d + ".hol_gt5"] = sum(1 for o in sl if o[d]["hol"] > 5) / k
            summ[name] = m
        # host part vs network part vs HOL
        def grp(o):
            h = sum(o[d][k] for d in ("up", "dn") for k in ("sched", "write", "sock"))
            q = sum(max(o[d]["queue"], 0) for d in ("up", "dn") if o[d]["ntx"] <= 1)
            l = sum(o[d]["hol"] + (o[d]["path"] - rtt / 2 if o[d]["ntx"] > 1 else 0) for d in ("up", "dn"))
            return h, q, l
        for name in ("slow5", "slow1", "all"):
            k = summ[name]["n"]
            sl = srt[-k:]
            g = [grp(o) for o in sl]
            summ[name]["host_ms"] = sum(x[0] for x in g) / k
            summ[name]["queue_ms"] = sum(x[1] for x in g) / k
            summ[name]["loss_hol_ms"] = sum(x[2] for x in g) / k
    json.dump({"summary": summ, "probes": out}, open(os.path.join(rd, "decomp.json"), "w"))
    print(json.dumps(summ, indent=1, default=lambda x: round(x, 2) if isinstance(x, float) else x))

if __name__ == "__main__":
    main()

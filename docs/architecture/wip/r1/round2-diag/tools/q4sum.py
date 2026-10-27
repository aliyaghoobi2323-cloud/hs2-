#!/usr/bin/env python3
"""q4sum.py OUT PHASE...: the TUN ping's round trip cut into parts (Q4 of
round 2, step 1; lab only), from the T/Q/W/R lines of both hs2 processes
and rigload's ping times. Per ping (ICMP id, seq), each direction (req:
edge -> exit, rep: exit -> edge), ms:

  tun     into hs2 from the TUN (req: rigload's send -> the edge's T line;
          rep: the exit parsed the request -> its T line of the reply: the
          exit's TUN write, the kernel's echo, the TUN read)
  host    T -> the record with it went into the link's socket (W's t_rec):
          the TUN side channel's queue, the scheduler, the shaper (with Q
          lines, A: l3 = T -> pushed, c2 = pushed -> taken, write = taken ->
          record)
  net     record -> the receiving hs2 parsed it (socket, bottleneck queue,
          head-of-line, wakeup)
  last    (rep) parsed by the edge -> rigload got it

and the link that carried it (the exit's port) with srtt - min_rtt at the
write and its bytes over the run (ss), against the other links."""
import glob, json, os, statistics, sys

sys.path.insert(0, "/root/hs2-wt/r1-diag/docs/architecture/wip/rig-scale")
import ana  # noqa

def icmp(key):
    for i in range(0, min(len(key) - 56, 120), 2):
        if key[i:i + 2] == "45" and key[i + 18:i + 20] == "01":
            return int(key[i + 40:i + 42], 16), int(key[i + 48:i + 52], 16), int(key[i + 52:i + 56], 16)
    return None

def med(v):
    v = [x for x in v if x is not None]
    return statistics.median(v) if v else None

def pct(v, q):
    v = sorted(x for x in v if x is not None)
    if not v:
        return None
    return v[min(len(v) - 1, int(round(q * (len(v) - 1))))]

def load(rd):
    tr = {}
    for p in glob.glob(os.path.join(rd, "trace", "trace-*.txt")):
        role = None
        L = {}
        for line in open(p, errors="replace"):
            t = line.split()
            if not t:
                continue
            if t[0] == "H":
                role = "edge" if "edge.json" in line else "exit"
                continue
            if t[0] not in "TQWR":
                continue
            n = int(t[2]) if t[0] == "T" else int(t[3])
            if n < 80 or n > 160:
                continue
            k = icmp(t[3] if t[0] == "T" else t[4])
            if not k:
                continue
            L.setdefault((t[0], k), []).append(t)
        tr[role] = L
    return tr

def one(L, kind, k):
    v = L.get((kind, k))
    return v[0] if v and len(v) == 1 else None

def run(rd):
    tr = load(rd)
    pr = json.load(open(os.path.join(rd, "probes.json")))
    t0 = pr["t0_ns"] / 1e9
    pu = [u for u in pr["users"] if u.get("ping")]
    if not pu or "edge" not in tr or "exit" not in tr:
        return None
    pg = pu[0]["ping"]
    samp = {round(s, 6): m for s, m in zip(pg["samp"], pg["ms"])}
    E, X = tr["edge"], tr["exit"]
    # the echo id: the one most T lines of the edge carry
    ids = {}
    for (kind, k) in E:
        if kind == "T" and k[0] == 8:
            ids[k[1]] = ids.get(k[1], 0) + 1
    if not ids:
        return None
    pid = max(ids, key=ids.get)
    rows = []
    for seq, s in enumerate(pg["send"]):
        if s < 5 or s >= 25:
            continue
        rtt = samp.get(round(s, 6))
        rq, rp = (8, pid, seq), (0, pid, seq)
        eT, eQ, eW, xR = one(E, "T", rq), one(E, "Q", rq), one(E, "W", rq), one(X, "R", rq)
        xT, xQ, xW, eR = one(X, "T", rp), one(X, "Q", rp), one(X, "W", rp), one(E, "R", rp)
        if rtt is None or not all((eT, eW, xR, xT, xW, eR)):
            rows.append({"seq": seq, "rtt": rtt, "ok": False})
            continue
        ts = lambda t, i: int(t[i]) / 1e9
        o = {"seq": seq, "rtt": rtt, "ok": True}
        for d, T, Q, W, R, prev in (("req", eT, eQ, eW, xR, t0 + s), ("rep", xT, xQ, xW, eR, ts(xR, 5))):
            p = {"tun": (ts(T, 4) - prev) * 1e3, "host": (ts(W, 6) - ts(T, 4)) * 1e3, "net": (ts(R, 5) - ts(W, 6)) * 1e3}
            if Q:
                p["l3"] = (ts(Q, 5) - ts(T, 4)) * 1e3
                p["c2"] = (ts(Q, 6) - ts(Q, 5)) * 1e3
                p["write"] = (ts(W, 6) - ts(Q, 6)) * 1e3
                p["lane"] = Q[7]
            if len(W) > 12:
                # W: ... t_write t_rec off lport rport srtt_us minrtt_us
                p["conn"] = int(W[8]) if d == "rep" else int(W[9])
                p["sq"] = (int(W[10]) - int(W[11])) / 1e3
            o[d] = p
        o["rep"]["last"] = (t0 + s + rtt / 1e3 - ts(eR, 5)) * 1e3
        rows.append(o)
    return rows

def links(rd):
    """bytes each link socket of the exit sent and the edge sent (ss, end)"""
    out = {}
    for side in ("exit", "edge"):
        for k, v in ana.ss_parse(os.path.join(rd, "ss-%s-end.txt" % side)).items():
            a, b = k.split()
            port = int(a.rsplit(":", 1)[1]) if side == "exit" else int(b.rsplit(":", 1)[1])
            out.setdefault(port, {})[side + "_MB"] = v.get("bytes_sent", 0) / 1e6
    return out

def main():
    out = sys.argv[1]
    res = {}
    for ph in sys.argv[2:]:
        for line in open(os.path.join(out, ph, "res.jsonl")):
            r = json.loads(line)
            if not r.get("ok") or r["MIX"] != "s5":
                continue
            rd = os.path.join(out, ph, "raw", r["id"])
            rows = run(rd)
            if rows is None:
                continue
            good = [x for x in rows if x["ok"]]
            m = ana.derive(r)
            L = links(rd)
            o = {"id": r["id"], "N": r["N"], "rate": r["RATE"], "bin": r["bin"], "pings": len(rows), "decomposed": len(good),
                 "t99": m.get("t99"), "tfloor": m.get("tfloor"), "p99": m.get("p99"), "floor": m.get("rtt_floor")}
            if good:
                srt = sorted(good, key=lambda x: x["rtt"])
                k = max(1, len(srt) // 20)
                for name, sl in (("all", srt), ("slow5", srt[-k:])):
                    o[name] = {d + "." + f: sum(x[d].get(f, 0) for x in sl) / len(sl)
                               for d in ("req", "rep") for f in ("tun", "host", "l3", "c2", "write", "net", "sq")}
                    o[name]["rtt"] = sum(x["rtt"] for x in sl) / len(sl)
                    o[name]["rep.last"] = sum(x["rep"]["last"] for x in sl) / len(sl)
                conns = {}
                for x in good:
                    for d in ("req", "rep"):
                        c = x[d].get("conn")
                        conns.setdefault((d, c), 0)
                        conns[(d, c)] += 1
                o["ping_links"] = {"%s:%s" % k: v for k, v in conns.items()}
                o["links_MB"] = {str(p): v for p, v in sorted(L.items())}
                o["lanes"] = sorted(set(x[d].get("lane") for x in good for d in ("req", "rep")) - {None})
            key = "%s/N%s/%s" % (r["RATE"], r["N"], r["bin"])
            res.setdefault(key, []).append(o)
            print(key, json.dumps(o, default=lambda v: round(v, 2) if isinstance(v, float) else v))
    json.dump(res, open(os.path.join(out, "q4sum.json"), "w"), indent=1, default=lambda v: round(v, 3) if isinstance(v, float) else v)

if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""q3sum.py OUT PHASE...: Q3 (P10 at N=1, 100 Mbit/s) of round 2, step 1.

Per configuration (median of the repeats [min..max]): (a) lone16 / capacity,
(b) the 1-connection user / its floor, (c) the bulk total / capacity, (e)
the existing bulk user after the join / its floor and its lowest second in
the first 5 s after the join / its floor; probe p99; the TCP payload the
exit's link socket carried (bytes_sent - bytes_retrans over the ss
snapshots) against the users' goodput over the same span (hs2's framing
and light traffic); retransmissions; and, from the exit's S lines (the
downloads' sender), the single bulk stream of the 1-connection (or the
existing) user: its rate, the share of the time its writer waited for the
peer's window, the peer window, the window / rate (the loop's round trip
as the window sees it)."""
import json, os, re, statistics, sys, glob

sys.path.insert(0, "/root/hs2-wt/r1-diag/docs/architecture/wip/rig-scale")
import ana  # noqa

def med(v):
    v = [x for x in v if x is not None]
    return statistics.median(v) if v else None

def rng(v):
    v = [x for x in v if x is not None]
    return (min(v), max(v)) if v else (None, None)

def sstime(rd, tag):
    try:
        return float(open(os.path.join(rd, "sstime-%s.txt" % tag)).read())
    except (OSError, ValueError):
        return None

def framing(r, rd):
    """users' goodput / the exit link socket's TCP payload over the ss span"""
    a = ana.ss_parse(os.path.join(rd, "ss-exit-start.txt"))
    b = ana.ss_parse(os.path.join(rd, "ss-exit-end.txt"))
    t0, t1 = sstime(rd, "start"), sstime(rd, "end")
    if not a or not b or not t0 or not t1:
        return None
    tcp = 0
    for k, v in b.items():
        u = a.get(k, {})
        tcp += (v.get("bytes_sent", 0) - v.get("bytes_retrans", 0)) - (u.get("bytes_sent", 0) - u.get("bytes_retrans", 0))
    cl = json.load(open(os.path.join(rd, "client.json")))
    c0 = cl["t0_unix_ms"] / 1e3
    dn = cl.get("dn_1s") or []
    s0, s1 = t0 - c0, t1 - c0
    good = 0.0
    for i, mb in enumerate(dn):
        lo, hi = max(i, s0), min(i + 1, s1)
        if hi > lo:
            good += mb * (hi - lo)
    tcp_mbit = tcp * 8 / 1e6 / (t1 - t0)
    good_mbit = good / (s1 - s0)
    return {"tcp_mbit": tcp_mbit, "good_mbit": good_mbit, "ratio": good_mbit / tcp_mbit if tcp_mbit else None}

def streams(rd, t_from, t_to, c0):
    """the exit's S lines: per stream, bytes, window waits, windows"""
    st = {}
    for p in glob.glob(os.path.join(rd, "trace", "trace-*.txt")):
        txt = open(p, errors="replace").read()
        if "exit.json" not in txt.split("\n", 1)[0]:
            continue
        for line in txt.splitlines():
            if not line.startswith("S "):
                continue
            t = line.split()
            ts = int(t[2]) / 1e9 - c0
            if not (t_from <= ts < t_to):
                continue
            sid = int(t[3])
            s = st.setdefault(sid, {"bytes": 0, "wait_ns": 0, "win": [], "infl": [], "n": 0})
            s["bytes"] += int(t[4])
            s["wait_ns"] += int(t[7])
            s["win"].append(int(t[5]))
            s["infl"].append(int(t[6]))
            s["n"] += 1
    return st

def main():
    out = sys.argv[1]
    rows = {}
    for ph in sys.argv[2:]:
        for line in open(os.path.join(out, ph, "res.jsonl")):
            r = json.loads(line)
            if not r.get("ok") or r["MIX"] == "s5":
                continue
            rd = os.path.join(out, ph, "raw", r["id"])
            m = ana.derive(r)
            x = {"m": m, "fr": framing(r, rd)}
            cl = json.load(open(os.path.join(rd, "client.json")))
            c0 = cl["t0_unix_ms"] / 1e3
            mix = r["MIX"]
            win = {"one_vs_16": (5, 35), "lone16": (5, 35), "newheavy": (5, 20)}[mix]
            st = streams(rd, win[0], win[1], c0)
            if st:
                sid, s = max(st.items(), key=lambda kv: kv[1]["bytes"])
                span = win[1] - win[0]
                x["one"] = {"sid": sid, "mbit": s["bytes"] * 8 / 1e6 / span, "wait": s["wait_ns"] / 1e9 / span,
                            "win_kib": statistics.median(s["win"]) / 1024, "infl_kib": statistics.median(s["infl"]) / 1024}
                x["one"]["loop_ms"] = (x["one"]["win_kib"] * 1024 * 8 / (x["one"]["mbit"] * 1e6) * 1e3) if x["one"]["mbit"] else None
            if mix == "newheavy":
                st2 = streams(rd, 25, 40, c0)
                if st and x.get("one") and x["one"]["sid"] in st2:
                    s = st2[x["one"]["sid"]]
                    x["one_after"] = {"mbit": s["bytes"] * 8 / 1e6 / 15, "wait": s["wait_ns"] / 1e9 / 15,
                                      "win_kib": statistics.median(s["win"]) / 1024}
                    x["one_after"]["loop_ms"] = x["one_after"]["win_kib"] * 1024 * 8 / (x["one_after"]["mbit"] * 1e6) * 1e3 if x["one_after"]["mbit"] else None
                ex = next(u for u in r["users"] if u["role"] == "exist")
                light = sum((u["dn_mbit"].get("after_settled", 0)) for u in r["users"] if u["role"] in ("light", "pinger"))
                fl = ana.fair_floor(m["cap"] - light, [None, None])[0]
                d1 = ex.get("dn_1s") or []
                first5 = d1[20:25]
                x["dip5"] = min(first5) / fl if first5 else None
                x["first5"] = first5
            rows.setdefault((mix, r["bin"]), []).append(x)
    res = {}
    for (mix, b), xs in sorted(rows.items()):
        M = lambda k: med([x["m"].get(k) for x in xs])
        R = lambda k: rng([x["m"].get(k) for x in xs])
        o = {"n": len(xs)}
        for k in ("a_ratio", "b_ratio", "c_ratio", "e_bulk_ratio", "p99", "rtt_floor", "rt_exit", "dn", "one",
                  "exist_before", "exist_after", "heavy_after", "p99_after", "cpu_exit", "cpu_edge"):
            if M(k) is not None:
                o[k] = (M(k), R(k))
        fr = [x["fr"] for x in xs if x["fr"]]
        if fr:
            o["framing"] = {k: med([f[k] for f in fr]) for k in ("tcp_mbit", "good_mbit", "ratio")}
        ones = [x["one"] for x in xs if x.get("one")]
        if ones:
            o["stream"] = {k: med([s[k] for s in ones]) for k in ("mbit", "wait", "win_kib", "infl_kib", "loop_ms")}
        oa = [x["one_after"] for x in xs if x.get("one_after")]
        if oa:
            o["stream_after"] = {k: med([s[k] for s in oa]) for k in ("mbit", "wait", "win_kib", "loop_ms")}
        if any(x.get("dip5") is not None for x in xs):
            o["dip5"] = (med([x.get("dip5") for x in xs]), rng([x.get("dip5") for x in xs]))
            o["first5"] = [x.get("first5") for x in xs]
        res["%s/%s" % (mix, b)] = o
        print(mix, b, json.dumps(o, default=lambda v: round(v, 3) if isinstance(v, float) else v))
    json.dump(res, open(os.path.join(out, "q3sum.json"), "w"), indent=1, default=lambda v: round(v, 4) if isinstance(v, float) else v)

if __name__ == "__main__":
    main()

#!/usr/bin/env python3
# sum63c.py [RESDIR]: rel63c A/B summary. Quiet tags MODE REP ARM (d11N); per (mode, arm): n, heavy median and
# values, crowd/call/game/echo p99 medians, max RSS. Dark/mixed: run hs133/ana133.py on the dk* dirs.
import json, os, sys, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/r63c/res"
G = {}
for t in sorted(os.listdir(R)):
    d = os.path.join(R, t)
    if t.startswith("dk") or not os.path.isdir(d): continue
    md, arm = t[:2], t[-1]
    try: r = json.load(open(os.path.join(d, "result.json")))
    except Exception: print(t, "MISSING"); continue
    h = r.get("heavy", {}); p = r.get("probe", {})
    if "error" in h: print(t, "iperf error (excluded):", str(h.get("error"))[:60]); continue
    rss = {"ir": 0, "kh": 0}
    try:
        for ln in open(os.path.join(d, "rss.txt")):
            q = ln.split(); rss[q[1]] = max(rss[q[1]], int(q[2]))
    except Exception: pass
    G.setdefault((md, arm), []).append((h.get("mbps", 0), p.get("crowd", {}).get("p99_ms", 0), p.get("call", {}).get("p99_ms", 0),
        p.get("game", {}).get("p99_ms", 0), p.get("echo_p99_ms", 0), p.get("crowd", {}).get("fail", 0), rss["ir"], rss["kh"]))
print("mode arm  n  heavy_med [values]                  crowd99 call99 game99 echo99 fails  rssmax ir/kh")
for k in sorted(G):
    v = G[k]; m = lambda i: st.median(x[i] for x in v)
    print("%-4s %-3s %2d %7.0f [%-28s] %6.1f %6.1f %6.1f %6.1f %5.0f  %d/%d" % (k[0], k[1], len(v), m(0), " ".join("%.0f" % x[0] for x in v),
        m(1), m(2), m(3), m(4), m(5), max(x[6] for x in v), max(x[7] for x in v)))
pan = []
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "panics.txt")
    if os.path.isfile(f):
        for ln in open(f):
            q = ln.split()
            if len(q) == 3 and q[2] != "0": pan.append(t + " " + ln.strip())
print("panic-like:", pan or "none")

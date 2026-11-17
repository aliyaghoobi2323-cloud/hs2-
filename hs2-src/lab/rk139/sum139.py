#!/usr/bin/env python3
# sum139.py [RESDIR]: one line per A/B run (heavy Mbit/s, crowd p99/fails, call/game/echo p99, links, RSS).
import json, os, sys
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/rk139/res"
rows = []
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "result.json")
    if not os.path.isfile(f): continue
    try: d = json.load(open(f))
    except Exception: rows.append((t[:2], t[2], t, None)); continue
    rows.append((t[:2], t[2], t, d))
for md, arm, t, d in sorted(rows):
    if d is None: print("%-6s MISSING" % t); continue
    p = d.get("probe", {}); c = p.get("crowd", {})
    print("%-6s %4s heavy %7.1f  crowd p99 %6.1f fail %d  call p99 %6.1f  game p99 %6.1f  echo p99 %6.1f  links %s  rss ir/kh %s/%s" % (
        t, arm, d.get("heavy", {}).get("mbps", 0), c.get("p99_ms", 0), c.get("fail", 0), p.get("call", {}).get("p99_ms", 0),
        p.get("game", {}).get("p99_ms", 0), p.get("echo_p99_ms", 0), d.get("links_end"), d.get("rss_ir_mb"), d.get("rss_kh_mb")))
# medians per mode+arm (heavy, crowd p99) and panic counts (panics.txt from d139.sh)
import statistics as st
G = {}
for md, arm, t, d in rows:
    if d is None or "error" in d.get("heavy", {}): continue   # iperf3 segfault etc: not a tunnel result
    G.setdefault((md, arm), []).append((d.get("heavy", {}).get("mbps", 0), d.get("probe", {}).get("crowd", {}).get("p99_ms", 0)))
for k in sorted(G):
    v = G[k]; print("%s %s n=%d heavy med %.0f [%s]  crowd p99 med %.1f" % (k[0], k[1], len(v), st.median(x[0] for x in v), " ".join("%.0f" % x[0] for x in v), st.median(x[1] for x in v)))
P = []
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "panics.txt")
    if os.path.isfile(f):
        for ln in open(f):
            p = ln.split()
            if len(p) == 3 and p[2] not in ("0", ""): P.append("%s %s" % (t, ln.strip()))
print("panic-like lines:", P or "none")

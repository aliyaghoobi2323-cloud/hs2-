#!/usr/bin/env python3
# sum60.py RESDIR: per ARM x MODE medians over the n runs of d60 (heavy Mbit/s, crowd/call/game/echo p99, fails).
import json, os, sys, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/rel60/res"
rows = {}
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "result.json")
    if not os.path.isfile(f): continue
    try: d = json.load(open(f))
    except Exception: continue
    m = open(os.path.join(R, t, "meta.txt")).read().split()
    arm = m[1].split("=")[1]; md = m[2].split("=")[1]
    p = d.get("probe", {}); c = p.get("crowd", {})
    rows.setdefault((md, arm), []).append((d.get("heavy", {}).get("mbps", 0), c.get("p99_ms", 0), c.get("fail", 0),
        p.get("call", {}).get("p99_ms", 0), p.get("game", {}).get("p99_ms", 0), p.get("echo_p99_ms", 0)))
print("mode arm n | heavy_med (runs) | crowd_p99_med | fails | call_p99 | game_p99 | echo_p99")
for (md, arm), v in sorted(rows.items()):
    med = lambda i: round(st.median(x[i] for x in v), 1)
    print(md, arm, len(v), "|", med(0), [round(x[0]) for x in v], "|", med(1), [round(x[1]) for x in v], "|", sum(x[2] for x in v), "|", med(3), "|", med(4), "|", med(5))

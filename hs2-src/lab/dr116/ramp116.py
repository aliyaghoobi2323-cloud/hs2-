#!/usr/bin/env python3
# ramp116.py RESDIR MODE: per arm, mean Mbit/s per iperf second (first 10 s) and mean of seconds 0-4 / 5-9.
import sys, os, json, statistics as st
R, M = sys.argv[1], sys.argv[2]; A = {}
for t in sorted(os.listdir(R)):
    if not t.startswith(M): continue
    try:
        h = json.load(open(os.path.join(R, t, "heavy.json")))
        arm = dict(x.split("=", 1) for x in open(os.path.join(R, t, "meta.txt")).read().split() if "=" in x)["ARM"]
        iv = [i["sum"]["bits_per_second"] / 1e6 for i in h["intervals"]]
    except Exception: continue
    if len(iv) >= 10: A.setdefault(arm, []).append(iv)
for a, v in sorted(A.items()):
    m = [round(st.mean(x[k] for x in v)) for k in range(10)]
    print(a, len(v), "per-second mean:", m, "| s0-4", round(st.mean(m[:5])), "s5-9", round(st.mean(m[5:])))

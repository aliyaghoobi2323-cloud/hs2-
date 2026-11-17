#!/usr/bin/env python3
# rev84.py RUN...: path d (192.168.53.1, steady 0.8 %) verdict reversals (lower<->ok), seconds lowered
import re, sys
for t in sys.argv[1:]:
    st, rev, low, last, lowt = "ok", 0, 0, None, 0.0
    try: lines = open(t + "/kh.log", errors="replace").read().splitlines()
    except Exception: continue
    for ln in lines:
        if "lb auto2: weight factor" not in ln: continue
        m = re.search(r"192\.168\.53\.1 ([0-9.]+)→([0-9.]+) \((\w+)", ln)
        if not m: continue
        h, mi, s = ln[:15].split(":"); ts = int(h) * 3600 + int(mi) * 60 + float(s)
        v = "ok" if float(m.group(2)) >= 1.0 and m.group(3) == "ok" else m.group(3)
        v = "lower" if v in ("lower", "cut") else "ok"
        if v != st:
            rev += 1
            if v == "lower": low += 1; lowt = ts
            else: last = ts
        st = v
    print("%-4s reversals %2d lowerings %2d end %s" % (t.split("/")[-1], rev, low, st))

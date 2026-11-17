#!/usr/bin/env python3
# rev89.py IP RUN...: one path's verdict reversals (lower<->ok) and weight-factor
# changes from the exit's "lb auto2: weight factor" lines (rev84.py with the
# path as an argument; lbv81 10-path setup: h = 192.168.57.1, steady 0.8 %).
import re, sys
ip = sys.argv[1]
pat = re.compile(re.escape(ip) + r" ([0-9.]+)→([0-9.]+) \((\w+)")
for t in sys.argv[2:]:
    st, rev, low, chg = "ok", 0, 0, 0
    try:
        lines = open(t + "/kh.log", errors="replace").read().splitlines()
    except Exception:
        continue
    for ln in lines:
        if "lb auto2: weight factor" not in ln:
            continue
        m = pat.search(ln)
        if not m:
            continue
        chg += 1
        v = "ok" if float(m.group(2)) >= 1.0 and m.group(3) == "ok" else m.group(3)
        v = "lower" if v in ("lower", "cut") else "ok"
        if v != st:
            rev += 1
            if v == "lower":
                low += 1
        st = v
    print("%-8s %s factor changes %3d reversals %3d lowerings %3d end %s" % (t.split("/")[-1], ip, chg, rev, low, st))

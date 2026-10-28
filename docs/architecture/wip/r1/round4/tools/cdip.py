#!/usr/bin/env python3
"""cdip.py RES.jsonl...: P10 (c) of each lone16 / one_vs_16 / newheavy run
(the bulk users' download over cap_P10, ana.py's) split into the seconds of
the main window: the median second, the seconds that dip under 0.85 of it
(BBRv1's PROBE_RTT on the lone BULK link at N=1: its window at 4 segments
for 200 ms and a round trip, each 10 s), the share they cost, and (c) with
those seconds at the median (what the framing and the queue leave)."""
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RIGSCALE", "/root/r4/rig"))
import ana  # noqa: E402

print("| run | (c) | median s / cap_P10 | dips (Mbit/s) | dips' cost | (c) without the dips |")
print("|---|---|---|---|---|---|")
for p in sys.argv[1:]:
    for l in open(p):
        if not l.strip():
            continue
        r = json.loads(l)
        if not r.get("ok") or r.get("MIX") not in ("lone16", "one_vs_16", "newheavy") or r.get("N") != 1:
            continue
        m = ana.derive(r)
        c = m.get("c_ratio")
        cap = ana.cap_p10(r)
        d1 = r.get("dn_1s") or []
        mw = [w for w in (r.get("windows") or []) if w.get("name") == "main"]
        a, b = (int(mw[0]["from"]), int(mw[0]["to"])) if mw else (5, len(d1))
        sec = d1[a:b]
        if not sec or c is None:
            continue
        med = statistics.median(sec)
        dips = [x for x in sec if x < 0.85 * med]
        cost = sum(med - x for x in dips) / (med * len(sec))
        print("| %s | %.3f | %s | %s | %.1f%% | %.3f |" % (
            r["id"], c, "%.1f / %.1f" % (med, cap) if cap else "%.1f" % med,
            " ".join("%.0f" % x for x in dips) or "-", 100 * cost, c / (1 - cost)))

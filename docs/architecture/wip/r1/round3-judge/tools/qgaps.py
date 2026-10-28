#!/usr/bin/env python3
"""qgaps.py REC.json...: the round-3 judge's look at the QUIET link's timing
(gamma1's fixed 4 ms hold against the candidate's jittered one): for the
connection that carries the least bytes of each capture (the QUIET link),
the gaps between its application records' bursts in the exit>edge
direction (records within 0.3 ms are one burst: one write), their
percentiles, and how peaked they are: the share of gaps in the most common
0.25 ms bin, and in the most common 1 ms window."""
import collections
import json
import sys

for p in sys.argv[1:]:
    recs = json.load(open(p))
    by = collections.defaultdict(int)
    for r in recs:
        by[r["c"]] += r["len"]
    cnt = collections.Counter(r["c"] for r in recs if r["type"] == 23 and r["d"] == "exit>edge")
    q = min((c for c in by if cnt[c] > 1000), key=by.get)
    ts = sorted(r["t"] for r in recs if r["c"] == q and r["d"] == "exit>edge" and r["type"] == 23)
    bursts = [ts[0]]
    for t in ts[1:]:
        if t - bursts[-1] > 0.0003:
            bursts.append(t)
    gaps = sorted((b - a) * 1000 for a, b in zip(bursts, bursts[1:]))
    n = len(gaps)
    pc = lambda x: gaps[min(n - 1, int(x * n))]
    bins = collections.Counter(int(g / 0.25) for g in gaps if g < 20)
    mode, mc = bins.most_common(1)[0]
    w = max(sum(bins.get(k + i, 0) for i in range(4)) for k in range(0, 80))
    print("%s: QUIET %s, %d records, %d bursts; gap ms p10 %.2f p25 %.2f p50 %.2f p75 %.2f p90 %.2f; mode bin %.2f-%.2f ms %.1f%% of gaps; best 1 ms window %.1f%%"
          % (p, q.split(">")[0].rsplit(":", 1)[1], len(ts), len(bursts), pc(.1), pc(.25), pc(.5), pc(.75), pc(.9),
             mode * 0.25, mode * 0.25 + 0.25, 100.0 * mc / n, 100.0 * w / n))

#!/usr/bin/env python3
"""idlerec.py REC.json...: the R1 round-4 judge's look at an idle tunnel (the
red team's Z10): for each capture (tlsrec.py's records of the link
connections), each connection's application records a second in each
direction over the idle span (the last IDLE s before the closing probe,
default 50), and the gaps between them (p10 / p50 / p90, s), connections
sorted by their records; a markdown table per capture."""
import json
import statistics
import sys

IDLE = 50.0


def pct(v, q):
    v = sorted(v)
    if not v:
        return float("nan")
    return v[min(len(v) - 1, int(q * len(v)))]


for p in sys.argv[1:]:
    recs = [r for r in json.load(open(p)) if r.get("type") == 23]
    if not recs:
        print("%s: no records" % p)
        continue
    t_end = max(r["t"] for r in recs)
    # the closing probe starts 2 s before the end: the idle span ends 3 s before
    t1 = t_end - 3.0
    t0 = t1 - IDLE
    conns = sorted({r["c"] for r in recs})
    rows = []
    for c in conns:
        row = [c]
        for d in ("exit>edge", "edge>exit"):
            ts = sorted(r["t"] for r in recs if r["c"] == c and r["d"] == d and t0 <= r["t"] < t1)
            gaps = [b - a for a, b in zip(ts, ts[1:])]
            row += [len(ts) / IDLE, pct(gaps, 0.1), pct(gaps, 0.5), pct(gaps, 0.9)]
        rows.append(row)
    rows.sort(key=lambda r: -(r[1] + r[5]))
    print("#### %s (idle %.0f s)\n" % (p.split("/")[-1], IDLE))
    print("| connection | exit>edge rec/s | gaps p10 / p50 / p90 s | edge>exit rec/s | gaps p10 / p50 / p90 s |")
    print("|---|---|---|---|---|")
    for r in rows:
        print("| %s | %.2f | %.2f / %.2f / %.2f | %.2f | %.2f / %.2f / %.2f |" % (r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7], r[8]))
    tot = [sum(r[1] for r in rows), sum(r[5] for r in rows)]
    print("\nall: %d connections, %.2f / %.2f records a second (exit>edge / edge>exit)\n" % (len(rows), tot[0], tot[1]))

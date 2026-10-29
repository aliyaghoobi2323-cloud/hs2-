#!/usr/bin/env python3
"""r5class.py RES.jsonl...: R1 round 5's class table, one per cell, one row
per binary; each figure the median of its runs with every run after it in
brackets (run order); "∞" where a percentile fell on a failed page or a lost
message (ana.py's -1 / inf), and a median that is ∞ when the upper middle
run is (ana.py's rule). From the red team's r2class.py (redteam2/tools).
Delays: one-way p99 of the main window less RTT/2. Columns: download sum
Mbit/s; TCP game p99 up / down; UDP game p99 down; call p99 up / down,
late+lost down %, delivered/offered (the lesser direction); video rebuffers;
web p95 page load, first byte p50 (ms), failed pages."""
import json
import math
import sys

LABEL = {
    "prod": "تولید", "fin2": "fin2", "ill": "fin2 + QUIET با Illinois",
}
INF = float("inf")


def load(paths):
    out = []
    for p in paths:
        for l in open(p):
            if l.strip():
                out.append(json.loads(l))
    return out


def fmt(v, p):
    if v is None:
        return "-"
    if v == INF:
        return "∞"
    return ("%%.%df" % p) % v


def med(vals, p=0):
    good = sorted(v for v in vals if v is not None)
    if not good:
        return "-"
    n = len(good)
    if n % 2:
        m = good[n // 2]
    else:
        a, b = good[n // 2 - 1], good[n // 2]
        m = INF if b == INF else (a + b) / 2
    return "%s (%s)" % (fmt(m, p), " ".join(fmt(v, p) for v in vals))


def get(r, *path):
    v = r
    for k in path:
        if not isinstance(v, dict):
            return None
        v = v.get(k)
    return v


def num(v):
    if v is None:
        return None
    if isinstance(v, str):
        return INF if v.startswith("inf") else None
    if isinstance(v, (int, float)) and v < 0:
        return INF
    return float(v)


def owd(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    v = num(w.get("p99"))
    return None if v is None else (v if v == INF else v - r["RTT"] / 2.0)


def late(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    if not w.get("sent"):
        return None
    return 100.0 * ((w.get("late") or 0) + (w.get("lost") or 0)) / w["sent"]


def deliv(r):
    out = None
    for d in ("up", "dn"):
        w = get(r, "classes", "call", d, "main") or {}
        if w.get("offered_mbit"):
            v = w.get("delivered_mbit", 0) / w["offered_mbit"]
            out = v if out is None else min(out, v)
    return out


def main():
    recs = load(sys.argv[1:])
    cells, order = {}, []
    for r in recs:
        if not r.get("ok"):
            continue
        c = r.get("cell")
        if c not in cells:
            cells[c] = {}
            order.append(c)
        cells[c].setdefault(r.get("bin"), []).append(r)
    for c in order:
        print("#### `%s`\n" % c)
        print("| باینری | دانلود Mbit/s | بازی TCP p99 بالا / پایین ms | بازی UDP p99 پایین | تماس p99 بالا / پایین ms | تماس دیر+گم پایین ٪ | رسیده/فرستاده | بازبافر | وب p95 ms | نخستین بایت p50 ms | صفحهٔ شکست‌خورده |")
        print("|---|---|---|---|---|---|---|---|---|---|---|")
        for b in sorted(cells[c], key=lambda b: list(LABEL).index(b) if b in LABEL else 99):
            rs = cells[c][b]
            row = [LABEL.get(b, b), med([num(get(r, "dn_sum_mbit", "main")) for r in rs], 1),
                   "%s / %s" % (med([owd(r, "game", "up") for r in rs]), med([owd(r, "game", "dn") for r in rs])),
                   med([owd(r, "gameudp", "dn") for r in rs]),
                   "%s / %s" % (med([owd(r, "call", "up") for r in rs]), med([owd(r, "call", "dn") for r in rs])),
                   med([late(r, "call", "dn") for r in rs], 1), med([deliv(r) for r in rs], 2),
                   med([num(get(r, "classes", "stream", "rebuffers")) for r in rs]),
                   med([num(get(r, "classes", "web", "main", "load_p95_ms")) for r in rs]),
                   med([num(get(r, "classes", "web", "main", "fb_p50_ms")) for r in rs]),
                   med([num(get(r, "classes", "web", "main", "failed")) for r in rs])]
            print("| " + " | ".join(row) + " |")
        print()


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""classtable.py RES.jsonl...: the round-3 judge's user-class tables (report
only), one per cell, one row per binary, each figure the median of the runs
with every run after it in brackets, in run order.

Delays are the one-way p99 of the main window less the path's one-way delay
(RTT/2), not less the class's own floor: a standing queue that raises every
message (the floor) is not hidden. Columns: the download sum; the TCP game's
p99 up / down and its late (past floor + 100 ms) or lost share down; the
same for the UDP game; the call's p99 up / down, its late (past floor + 150
ms) or lost share down and delivered/offered down; the viewers' start p50 /
max (s), rebuffers and viewers that never started; the web's page load p95
and first byte p50 (ms)."""
import json
import statistics
import sys

sys.path.insert(0, "/root/r3j/tools")
from j3table import BINORD, label  # noqa: E402


def load(paths):
    recs = []
    for p in paths:
        for l in open(p):
            if l.strip():
                recs.append(json.loads(l))
    return recs


def med(vals, p=0):
    good = [v for v in vals if v is not None]
    if not good:
        return "-"
    fmt = "%%.%df" % p
    m = statistics.median(good)
    return "%s (%s)" % (fmt % m, " ".join("-" if v is None else fmt % v for v in vals))


def owd(r, kind, d):
    c = (r.get("classes") or {}).get(kind)
    if not c:
        return None
    w = (c.get(d) or {}).get("main") or {}
    if w.get("p99") is None:
        return None
    return w["p99"] - r["RTT"] / 2.0


def lateshare(r, kind, d):
    c = (r.get("classes") or {}).get(kind)
    if not c:
        return None
    w = (c.get(d) or {}).get("main") or {}
    if not w.get("sent"):
        return None
    return 100.0 * ((w.get("late") or 0) + (w.get("lost") or 0)) / w["sent"]


def delivered(r):
    c = (r.get("classes") or {}).get("call")
    if not c:
        return None
    w = (c.get("dn") or {}).get("main") or {}
    if not w.get("offered_mbit"):
        return None
    return w.get("delivered_mbit", 0) / w["offered_mbit"]


def get(r, *path):
    v = r
    for k in path:
        if not isinstance(v, dict):
            return None
        v = v.get(k)
    return v


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
        print("| باینری | دانلود Mbit/s | بازی TCP p99 بالا / پایین ms | دیر+گم پایین ٪ | بازی UDP p99 بالا / پایین ms | دیر+گم پایین ٪ | تماس p99 بالا / پایین ms | دیر+گم پایین ٪ | رسیده/فرستاده | ویدیو آغاز p50 / بیشینه s | بازبافر | آغازنکرده | وب p95 بارگذاری ms | نخستین بایت p50 ms |")
        print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for b in sorted(cells[c], key=lambda b: BINORD.index(b) if b in BINORD else 99):
            rs = cells[c][b]
            row = [label(b), med([get(r, "dn_sum_mbit", "main") for r in rs], 1)]
            for k in ("game", "gameudp"):
                row.append("%s / %s" % (med([owd(r, k, "up") for r in rs]), med([owd(r, k, "dn") for r in rs])))
                row.append(med([lateshare(r, k, "dn") for r in rs], 1))
            row.append("%s / %s" % (med([owd(r, "call", "up") for r in rs]), med([owd(r, "call", "dn") for r in rs])))
            row.append(med([lateshare(r, "call", "dn") for r in rs], 1))
            row.append(med([delivered(r) for r in rs], 2))
            row.append("%s / %s" % (med([get(r, "classes", "stream", "startup_p50_s") for r in rs], 1),
                                     med([get(r, "classes", "stream", "startup_max_s") for r in rs], 1)))
            row.append(med([get(r, "classes", "stream", "rebuffers") for r in rs]))
            row.append(med([get(r, "classes", "stream", "not_started") for r in rs]))
            row.append(med([get(r, "classes", "web", "main", "load_p95_ms") for r in rs]))
            row.append(med([get(r, "classes", "web", "main", "fb_p50_ms") for r in rs]))
            print("| " + " | ".join(row) + " |")
        print()


if __name__ == "__main__":
    main()

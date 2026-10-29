#!/usr/bin/env python3
"""r2class.py RES.jsonl...: the R1 red team's (second pass) class table, one
per cell, one row per binary; each figure the median of its runs with every
run after it in brackets (run order). Delays: one-way p99 of the main window
less RTT/2. Columns: download sum Mbit/s; TCP game p99 up / down; call p99 up
/ down, late+lost down %, delivered/offered down; video rebuffers; web p95
page load, first byte p50 (ms), failed pages."""
import json
import statistics
import sys

LABEL = {
    "prod": "تولید", "fin2": "fin2", "j3g": "j3g", "r2": "r2 (اصلاح‌ها روشن)", "r2off": "r2 (اصلاح‌ها خاموش)",
    "r2gs": "r2 خاموش + gostat", "qbbr": "fin2 + QUIET با BBR", "qf2": "fin2 + دو QUIET", "q2s": "fin2 + کف دو قطعه‌ای QUIET",
}
ORDER = list(LABEL)


def load(paths):
    out = []
    for p in paths:
        for l in open(p):
            if l.strip():
                out.append(json.loads(l))
    return out


def med(vals, p=0):
    good = [v for v in vals if v is not None]
    if not good:
        return "-"
    f = "%%.%df" % p
    return "%s (%s)" % (f % statistics.median(good), " ".join("-" if v is None else f % v for v in vals))


def get(r, *path):
    v = r
    for k in path:
        if not isinstance(v, dict):
            return None
        v = v.get(k)
    return v


def owd(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    return None if w.get("p99") is None else w["p99"] - r["RTT"] / 2.0


def late(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    if not w.get("sent"):
        return None
    return 100.0 * ((w.get("late") or 0) + (w.get("lost") or 0)) / w["sent"]


def deliv(r):
    w = get(r, "classes", "call", "dn", "main") or {}
    return None if not w.get("offered_mbit") else w.get("delivered_mbit", 0) / w["offered_mbit"]


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
        print("| باینری | دانلود Mbit/s | بازی TCP p99 بالا / پایین ms | تماس p99 بالا / پایین ms | تماس دیر+گم پایین ٪ | رسیده/فرستاده | بازبافر | وب p95 ms | نخستین بایت p50 ms | صفحهٔ شکست‌خورده |")
        print("|---|---|---|---|---|---|---|---|---|---|")
        for b in sorted(cells[c], key=lambda b: ORDER.index(b) if b in ORDER else 99):
            rs = cells[c][b]
            row = [LABEL.get(b, b), med([get(r, "dn_sum_mbit", "main") for r in rs], 1),
                   "%s / %s" % (med([owd(r, "game", "up") for r in rs]), med([owd(r, "game", "dn") for r in rs])),
                   "%s / %s" % (med([owd(r, "call", "up") for r in rs]), med([owd(r, "call", "dn") for r in rs])),
                   med([late(r, "call", "dn") for r in rs], 1), med([deliv(r) for r in rs], 2),
                   med([get(r, "classes", "stream", "rebuffers") for r in rs]),
                   med([get(r, "classes", "web", "main", "load_p95_ms") for r in rs]),
                   med([get(r, "classes", "web", "main", "fb_p50_ms") for r in rs]),
                   med([get(r, "classes", "web", "main", "failed") for r in rs])]
            print("| " + " | ".join(row) + " |")
        print()


if __name__ == "__main__":
    main()

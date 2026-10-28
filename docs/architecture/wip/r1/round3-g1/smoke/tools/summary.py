#!/usr/bin/env python3
"""summary.py MERGED.jsonl: per cell and binary, the medians the T19 goals
are judged on (ana.derive), and each binary's throughput against the base
(old) of the same cell: dn and up ratios of the medians. Markdown."""
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RIGSCALE", "/root/r3a/rig"))
import ana  # noqa: E402

ORDER = ["old", "b2nx", "g1", "g1w", "g1e", "g1et"]


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def f(x, p=1):
    return "-" if x is None else ("%%.%df" % p) % x


cells, order = {}, []
for l in open(sys.argv[1]):
    if not l.strip():
        continue
    r = json.loads(l)
    if not r.get("ok"):
        continue
    c = r["cell"]
    if c not in cells:
        cells[c] = {}
        order.append(c)
    cells[c].setdefault(r["bin"], []).append(ana.derive(r))

print("| خانه | باینری | n | دانلود | آپلود | دانلود/پایه | آپلود/پایه | (الف) | (ب) | (ج) | (هـ) حجیم | کاوشگر−کف | پینگ TUN−کف | بازارسال خ/ل ٪ |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
for c in order:
    bins = sorted(cells[c], key=lambda b: ORDER.index(b) if b in ORDER else 99)
    base = cells[c].get("old")
    bdn = med([m.get("dn") for m in base]) if base else None
    bup = med([m.get("up") for m in base]) if base else None
    short = c.replace(" RTT=72", "").replace(" MODE=fqcodel", " fq").replace(" USERS=", " U").replace(" MIX=", " ")
    for b in bins:
        ms = cells[c][b]
        dn, up = med([m.get("dn") for m in ms]), med([m.get("up") for m in ms])
        rdn = dn / bdn if bdn and dn is not None else None
        rup = up / bup if bup and up and up > 0.5 else None
        p = med([m["p99"] - m["rtt_floor"] for m in ms if "p99" in m])
        t = med([m["t99"] - m["tfloor"] for m in ms if "t99" in m])
        print("| %s | %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s |" % (
            short, b, len(ms), f(dn), f(up), f(rdn, 3), f(rup, 3),
            f(med([m.get("a_ratio") for m in ms]), 3), f(med([m.get("b_ratio") for m in ms]), 3),
            f(med([m.get("c_ratio") for m in ms]), 3), f(med([m.get("e_bulk_ratio") for m in ms]), 3),
            f(p), f(t), f(med([m.get("rt_exit") for m in ms]), 2), f(med([m.get("rt_edge") for m in ms]), 2)))

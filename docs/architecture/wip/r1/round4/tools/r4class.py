#!/usr/bin/env python3
"""r4class.py RES.jsonl... [--md]: R1 round 4's user-class table (report
only; the round-3 judge's classtable.py with this round's labels), one per
cell, one row per binary, each figure the median of its runs with every run
after it in brackets (run order).

Delays are the one-way p99 of the main window less RTT/2 (the path's one-way
delay), not less the class's own floor. Columns: download sum; TCP game p99
up / down and late+lost down; UDP game the same; call p99 up / down,
late+lost down and delivered/offered down; viewers' start p50 / max (s),
rebuffers, never started, segment rate (Mbit/s, mean of viewers); web load
p95 and first byte p50 (ms)."""
import json
import statistics
import sys

LABEL = {
    "prod": "تولید (1c23629)", "j3g": "j3g (پایهٔ R1)", "base": "پایه (r1-judge)", "b2nx": "نهایی دور ۲ (b2nx)",
    "r4": "r4، همه خاموش", "xs": "سهم میان‌لینکی (xs)", "pl": "جای‌گذاری با کاربران (pl)", "xp": "xs + pl",
    "qc": "سقف QUIET (qc)", "rt": "لاین بی‌درنگ (rt)", "qr": "qc + rt", "tw": "دوسویه (tw)",
    "twqc": "tw + qc", "twrt": "tw + rt", "twqr": "tw + qc + rt",
    "pl2": "جای‌گذاری کاربر سنگین (pl2)", "tw3": "دوسویهٔ نرم (tw3)", "twrt3": "tw3 + rt", "twqr3": "tw3 + qc + rt", "ca": "pl2 + tw3 + qc + rt",
    "c4": "نامزد دور ۴ (c4)", "c4nort": "c4 بی لاین بی‌درنگ", "c4nopl": "c4 بی جای‌گذاری", "c4notw": "c4 بی دوسویه و بی‌درنگ",
}
ORDER = list(LABEL)


def label(b):
    return LABEL.get(b, b)


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


def get(r, *path):
    v = r
    for k in path:
        if not isinstance(v, dict):
            return None
        v = v.get(k)
    return v


def owd(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    if w.get("p99") is None:
        return None
    return w["p99"] - r["RTT"] / 2.0


def lateshare(r, kind, d):
    w = get(r, "classes", kind, d, "main") or {}
    if not w.get("sent"):
        return None
    return 100.0 * ((w.get("late") or 0) + (w.get("lost") or 0)) / w["sent"]


def delivered(r):
    w = get(r, "classes", "call", "dn", "main") or {}
    if not w.get("offered_mbit"):
        return None
    return w.get("delivered_mbit", 0) / w["offered_mbit"]


def main():
    recs = load([a for a in sys.argv[1:] if not a.startswith("--")])
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
        print("| باینری | دانلود Mbit/s | بازی TCP p99 بالا / پایین ms | دیر+گم پایین ٪ | بازی UDP p99 بالا / پایین ms | دیر+گم پایین ٪ | تماس p99 بالا / پایین ms | دیر+گم پایین ٪ | رسیده/فرستاده | ویدیو آغاز p50 / بیشینه s | بازبافر | آغازنکرده | نرخ قطعه Mbit/s | وب p95 بارگذاری ms | نخستین بایت p50 ms |")
        print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for b in sorted(cells[c], key=lambda b: ORDER.index(b) if b in ORDER else 99):
            rs = cells[c][b]
            hp = sum(1 for r in rs if r.get("host_pressure"))
            row = [label(b) + (" ⚠ فشار حافظهٔ میزبان در %d اجرا" % hp if hp else ""), med([get(r, "dn_sum_mbit", "main") for r in rs], 1)]
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
            row.append(med([get(r, "classes", "stream", "seg_mbit_mean") for r in rs], 2))
            row.append(med([get(r, "classes", "web", "main", "load_p95_ms") for r in rs]))
            row.append(med([get(r, "classes", "web", "main", "fb_p50_ms") for r in rs]))
            print("| " + " | ".join(row) + " |")
        print()


if __name__ == "__main__":
    main()

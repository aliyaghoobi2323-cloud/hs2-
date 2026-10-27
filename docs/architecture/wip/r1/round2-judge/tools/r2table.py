#!/usr/bin/env python3
"""r2table.py RES.jsonl... : the round-2 judge's tables (markdown), one per
cell, one row per binary label, every repeat listed (in run order), the
verdict on the median as ana.py takes it (T17: P10 on cap_P10; lost probes
and pings count). Also: r2table.py --ramp RES.jsonl... (per-second download
sum of every run: the ramp after start) and --json (per-run metrics)."""
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RIGSCALE", "/root/hs2-wt/r1-r2j/docs/architecture/wip/rig-scale"))
import ana  # noqa: E402

LABEL = {"old": "پایه", "b1": "β1", "b1na": "β1 بی α", "b2": "β2", "b2na": "β2 بی α",
         "b2f": "وصله ن۱", "b2g": "وصله ن۲", "b2h": "وصله ن۳", "b2i": "وصله ن۴",
         "b2jx": "ن۴ + QUIET بیرون (b2jx)", "b2jxr": "b2jx + رکورد تعاملی",
         "b2kx": "ن۴ + FIFO (b2kx)", "b2kxr": "b2kx + رکورد تعاملی", "b2kxn": "b2kx بی تنظیم FIFO",
         "b2mx": "ن۴ + FIFO + شروع (b2mx)", "b2mxr": "b2mx + رکورد تعاملی",
         "b2nx": "نهایی ن۵ (b2nx)", "b2nxr": "نهایی + رکورد تعاملی"}
BINORD = ["old", "b1", "b1na", "b2", "b2na", "b2f", "b2g", "b2h", "b2i", "b2jx", "b2jxr", "b2kxn", "b2kx", "b2kxr",
          "b2mx", "b2mxr", "b2nx", "b2nxr"]


def label(b):
    return LABEL.get(b, b)


def load(paths):
    recs = []
    for p in paths:
        for l in open(p):
            if l.strip():
                recs.append(json.loads(l))
    return recs


def f(x, p=1):
    return "-" if x is None else ("%%.%df" % p) % x


def reps(ms, key, p=1, scale=1.0, sub=None):
    v = []
    for m in ms:
        x = m.get(key)
        if x is not None and sub:
            y = m.get(sub)
            x = None if y is None else x - y
        v.append(None if x is None else x * scale)
    good = [x for x in v if x is not None]
    if not good:
        return "-", None
    med = statistics.median(good)
    return "%s (%s)" % (f(med, p), " ".join(f(x, p) for x in v)), med


def verdict(ok_runs, n, med_ok):
    if med_ok is None:
        return "-"
    return "%s %d/%d" % ("PASS" if med_ok else "FAIL", ok_runs, n)


COMPACT = "--compact" in sys.argv


def table(recs):
    cells, order = {}, []
    for r in recs:
        c = r.get("cell")
        if c not in cells:
            cells[c] = {}
            order.append(c)
        cells[c].setdefault(r.get("bin"), []).append(r)
    out = []
    for c in order:
        out.append("#### `%s`\n" % c)
        if COMPACT:
            out.append("| باینری | دانلود Mbit/s | آپلود Mbit/s | (ج) | (ب) | کاوشگر − کف ms | (د) | پینگ TUN − کف ms | tun | بازارسال خروجی/لبه ٪ |")
            out.append("|---|---|---|---|---|---|---|---|---|---|")
        else:
            out.append("| باینری | اجرا | دانلود Mbit/s | آپلود Mbit/s | (ج) به cap_P10 | (ج) خام | (ب) به cap_P10 | p99 کاوشگر − کف ms | کران | (د) | پینگ TUN − کف ms | کران | tun | گم‌شده کاوشگر/پینگ | بازارسال خروجی/لبه ٪ | CPU لبه/خروجی ٪ | پرکاری ٪ |")
            out.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        bins = sorted(cells[c], key=lambda b: BINORD.index(b) if b in BINORD else 99)
        for b in bins:
            rs = [r for r in cells[c][b] if r.get("ok")]
            ms = [ana.derive(r) for r in rs]
            n = len(rs)
            if not n:
                out.append("| %s | 0/%d | | | | | | | | | | | | | | | |" % (label(b), len(cells[c][b])))
                continue
            dn, _ = reps(ms, "dn")
            up, _ = reps(ms, "up")
            cc, ccm = reps(ms, "c_ratio", 3)
            cr, _ = reps(ms, "c_raw", 3)
            bb, bbm = reps(ms, "b_ratio", 3)
            pd, pdm = reps(ms, "p99", 1, sub="rtt_floor")
            bd = [m["d_bound"] - m["rtt_floor"] for m in ms if "d_bound" in m]
            td, tdm = reps(ms, "t99", 1, sub="tfloor")
            tb = [m["t_bound"] - m["tfloor"] for m in ms if "t_bound" in m]
            plost = statistics.median([m.get("plost_frac", 0) for m in ms]) if ms else 0
            tlost = statistics.median([m.get("tlost_frac", 0) for m in ms if "tlost_frac" in m]) if any("tlost_frac" in m for m in ms) else None
            dv = None
            if pdm is not None and bd:
                dv = pdm <= statistics.median(bd) and plost <= ana.LOST_MAX
            tv = None
            if tdm is not None and tb:
                tv = tdm <= statistics.median(tb) and (tlost or 0) <= ana.LOST_MAX
            rt_x, _ = reps(ms, "rt_exit", 2)
            rt_e, _ = reps(ms, "rt_edge", 2)
            ce = statistics.median([m["cpu_edge"] for m in ms if m.get("cpu_edge") is not None] or [0])
            cx = statistics.median([m["cpu_exit"] for m in ms if m.get("cpu_exit") is not None] or [0])
            busy = max((m.get("busy") or 0) for m in ms)
            if COMPACT:
                out.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s |" % (
                    label(b), dn, up, cc, bb, pd, verdict(sum(1 for m in ms if m.get("d")), n, dv),
                    td, verdict(sum(1 for m in ms if m.get("tun")), n, tv), rt_x.split(" ")[0], rt_e.split(" ")[0]))
                continue
            out.append("| %s | %d/%d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %.0f / %.0f | %.0f |" % (
                label(b), n, len(cells[c][b]), dn, up, cc, cr, bb, pd,
                f(statistics.median(bd), 0) if bd else "-", verdict(sum(1 for m in ms if m.get("d")), n, dv),
                td, f(statistics.median(tb), 0) if tb else "-", verdict(sum(1 for m in ms if m.get("tun")), n, tv),
                "%.0f%% / %s" % (100 * plost, "-" if tlost is None else "%.0f%%" % (100 * tlost)),
                rt_x.split(" ")[0], rt_e.split(" ")[0], ce, cx, busy))
        out.append("")
    return "\n".join(out)


def ramp(recs):
    out = []
    for r in recs:
        if not r.get("ok"):
            continue
        d = r.get("dn_1s") or []
        u = r.get("up_1s") or []
        out.append("%s %-5s %s\n   dn %s\n   up %s" % (r.get("id"), r.get("bin"), r.get("cell"),
                                                     " ".join("%.0f" % x for x in d), " ".join("%.0f" % x for x in u)))
    return "\n".join(out)


def main():
    a = [x for x in sys.argv[1:] if x != "--compact"]
    if a and a[0] == "--ramp":
        print(ramp(load(a[1:])))
    elif a and a[0] == "--json":
        recs = load(a[1:])
        for r in recs:
            m = ana.derive(r) if r.get("ok") else {}
            print(json.dumps({"id": r.get("id"), "bin": r.get("bin"), "cell": r.get("cell"), "m": m}))
    else:
        print(table(load(a)))


if __name__ == "__main__":
    main()

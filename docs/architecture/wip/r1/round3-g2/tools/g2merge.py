#!/usr/bin/env python3
"""g2merge.py BLOCK=RES.jsonl ... [--cells FILE]: γ2's merged (the round-2 judge's r2merge.py)
tables for the report: one table per cell over every block that ran it, one
row per block and binary (every repeat listed, the median first), compact
columns. --cells: the cells to print, one per line, in that order (default:
all, in the order first met)."""
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RIGSCALE", "/root/r3b/rig"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ana  # noqa: E402
import g2table as rt  # noqa: E402


def main():
    args = sys.argv[1:]
    cells_file = None
    if "--cells" in args:
        i = args.index("--cells")
        cells_file = args[i + 1]
        del args[i:i + 2]
    rows = {}
    order = []
    for a in args:
        blk, path = a.split("=", 1)
        for l in open(path):
            if not l.strip():
                continue
            r = json.loads(l)
            c = r.get("cell")
            if c not in rows:
                rows[c] = []
                order.append(c)
            rows[c].append((blk, r))
    if cells_file:
        order = [l.strip() for l in open(cells_file) if l.strip() and not l.startswith("#")]
    out = []
    for c in order:
        if c not in rows:
            out.append("#### `%s`\n\n(اجرا نشد)\n" % c)
            continue
        out.append("#### `%s`\n" % c)
        out.append("| بلوک | باینری | دانلود Mbit/s | آپلود Mbit/s | (ج) | (ب) | کاوشگر − کف ms | (د) | پینگ TUN − کف ms | tun | بازارسال خروجی/لبه ٪ |")
        out.append("|---|---|---|---|---|---|---|---|---|---|---|")
        groups = {}
        gorder = []
        for blk, r in rows[c]:
            k = (blk, r.get("bin"))
            if k not in groups:
                groups[k] = []
                gorder.append(k)
            groups[k].append(r)
        gorder.sort(key=lambda k: (rt.BINORD.index(k[1]) if k[1] in rt.BINORD else 99, k[0]))
        for blk, b in gorder:
            rs = [r for r in groups[(blk, b)] if r.get("ok")]
            ms = [ana.derive(r) for r in rs]
            if not ms:
                continue
            n = len(ms)
            dn, _ = rt.reps(ms, "dn")
            up, _ = rt.reps(ms, "up")
            cc, _ = rt.reps(ms, "c_ratio", 3)
            bb, _ = rt.reps(ms, "b_ratio", 3)
            if "a_ratio" in ms[0]:
                bb, _ = rt.reps(ms, "a_ratio", 3)
                bb = "(الف) " + bb
            pd, pdm = rt.reps(ms, "p99", 1, sub="rtt_floor")
            bd = [m["d_bound"] - m["rtt_floor"] for m in ms if "d_bound" in m]
            td, tdm = rt.reps(ms, "t99", 1, sub="tfloor")
            tb = [m["t_bound"] - m["tfloor"] for m in ms if "t_bound" in m]
            plost = statistics.median([m.get("plost_frac", 0) for m in ms])
            tl = [m["tlost_frac"] for m in ms if "tlost_frac" in m]
            tlost = statistics.median(tl) if tl else 0
            dv = None if pdm is None or not bd else (pdm <= statistics.median(bd) and plost <= ana.LOST_MAX)
            tv = None if tdm is None or not tb else (tdm <= statistics.median(tb) and tlost <= ana.LOST_MAX)
            rx, _ = rt.reps(ms, "rt_exit", 2)
            re_, _ = rt.reps(ms, "rt_edge", 2)
            out.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s |" % (
                blk, rt.label(b), dn, up, cc, bb, pd, rt.verdict(sum(1 for m in ms if m.get("d")), n, dv),
                td, rt.verdict(sum(1 for m in ms if m.get("tun")), n, tv), rx.split(" ")[0], re_.split(" ")[0]))
        out.append("")
    print("\n".join(out))


if __name__ == "__main__":
    main()

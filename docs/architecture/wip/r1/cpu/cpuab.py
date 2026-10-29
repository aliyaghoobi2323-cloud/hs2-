#!/usr/bin/env python3
"""cpuab.py OUT [RIG_CPUS] [--json] — the CPU-split A/B table (R1 speed).

OUT holds one run.sh output a condition (OUT/A quiet, OUT/B pinned rig +
load, OUT/C unpinned rig + load; ab.sh). For each cell and metric: the median
and the [min..max] of the repeats in each condition, the change of each
condition's median against A, the noise (the larger repeat-to-repeat range of
the two conditions compared) and the verdict:
  throughput (Mbit/s): |delta| <= max(1 % of A's median, noise)
  latency (ms, p99 above the run's floor): delta <= max(1 ms, noise)
Also each condition's CPU picture in the measuring window: the busy share of
the rig's CPUs (RIG_CPUS, 0-5) and of the others, their softirq share, and
the steal share (the hypervisor), means over the runs.
"""
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RIGDIR", "/root/hs2-wt/r1-cpu/docs/architecture/wip/rig-scale"))
import ana  # noqa: E402

THR, LAT = "thr", "lat"


def metrics(rec, m):
    """(name, kind, value) of a run, by its mix"""
    mix = rec["MIX"]
    out = []
    if "p99" in m and "rtt_floor" in m:
        out.append(("probe p99-floor", LAT, m["p99"] - m["rtt_floor"]))
    if "t99" in m and "tfloor" in m:
        out.append(("TUN ping p99-floor", LAT, m["t99"] - m["tfloor"]))
    if mix == "lone16":
        out.append(("lone user", THR, m["a_ratio"] * m["cap_p10"]))
    if mix == "one_vs_16":
        out.append(("total dn", THR, m["dn"]))
        out.append(("1-conn user", THR, m["one"]))
    if mix == "tunping":
        out.append(("TUN dn", THR, m.get("tun_dn") or 0.0))
    if mix == "gamer_vs_dl":
        for k in ("game", "gameudp"):
            for d in ("up", "dn"):
                if m.get("%s_%s_p99" % (k, d)) is not None:
                    out.append(("%s %s p99-floor" % (k, d), LAT, m["%s_%s_p99" % (k, d)]))
        out.append(("heavy mean", THR, m.get("heavy_mean", 0.0)))
    return out


def cpulist(s):
    r = set()
    for p in s.split(","):
        a, _, b = p.partition("-")
        r.update(range(int(a), int(b or a) + 1))
    return r


def load(path):
    recs = []
    try:
        for l in open(path):
            l = l.strip()
            if l:
                recs.append(json.loads(l))
    except OSError:
        pass
    return recs


def main():
    a = [x for x in sys.argv[1:] if not x.startswith("--")]
    as_json = "--json" in sys.argv
    out = a[0]
    rig = cpulist(a[1] if len(a) > 1 else "0-5")
    conds = [c for c in ("A", "B", "C") if os.path.exists(os.path.join(out, c, "res.jsonl"))]
    data = {}   # cell -> cond -> metric -> [values]
    kinds = {}
    cpu = {}    # cond -> lists
    fails = {}
    for c in conds:
        for rec in load(os.path.join(out, c, "res.jsonl")):
            cell = " ".join(t for t in rec.get("cell", "").split() if not t.startswith("BIN="))
            if not rec.get("ok"):
                fails.setdefault(c, []).append("%s: %s" % (rec.get("id"), "; ".join(rec.get("errs", []))))
                continue
            m = ana.derive(rec)
            for name, kind, v in metrics(rec, m):
                data.setdefault(cell, {}).setdefault(c, {}).setdefault(name, []).append(v)
                kinds[name] = kind
            cs = rec.get("cpus")
            if cs:
                n = len(cs["busy"])
                ri = [i for i in range(n) if i in rig]
                oi = [i for i in range(n) if i not in rig]
                p = cpu.setdefault(c, {"rig": [], "other": [], "sirq_rig": [], "sirq_other": [], "steal": [], "host": []})
                p["rig"].append(statistics.mean(cs["busy"][i] for i in ri))
                p["other"].append(statistics.mean(cs["busy"][i] for i in oi))
                p["sirq_rig"].append(statistics.mean(cs["softirq"][i] for i in ri))
                p["sirq_other"].append(statistics.mean(cs["softirq"][i] for i in oi))
                p["steal"].append(statistics.mean(cs["steal"]))
                p["host"].append(rec.get("host_busy_pct") or 0)
    res = {"cells": [], "cpu": {}, "fails": fails}
    allpass = {c: True for c in conds if c != "A"}
    for cell, cd in data.items():
        rows = []
        for name in sorted({n for c in cd for n in cd[c]}, key=lambda n: (kinds[n] != THR, n)):
            row = {"metric": name, "kind": kinds[name]}
            for c in conds:
                v = sorted(cd.get(c, {}).get(name, []))
                if v:
                    row[c] = {"med": statistics.median(v), "min": v[0], "max": v[-1], "n": len(v)}
            if "A" in row:
                for c in conds:
                    if c == "A" or c not in row:
                        continue
                    d = row[c]["med"] - row["A"]["med"]
                    noise = max(row["A"]["max"] - row["A"]["min"], row[c]["max"] - row[c]["min"])
                    if kinds[name] == THR:
                        tol = max(0.01 * row["A"]["med"], noise)
                        ok = abs(d) <= tol
                    else:
                        tol = max(1.0, noise)
                        ok = d <= tol
                    row["d" + c] = {"delta": d, "noise": noise, "tol": tol, "pass": ok}
                    allpass[c] = allpass[c] and ok
            rows.append(row)
        res["cells"].append({"cell": cell, "rows": rows})
    for c, p in cpu.items():
        res["cpu"][c] = {k: round(statistics.mean(v), 1) for k, v in p.items() if v}
        res["cpu"][c]["runs"] = len(p["rig"])
    res["pass"] = allpass
    if as_json:
        print(json.dumps(res, indent=1, ensure_ascii=False))
        return
    for cl in res["cells"]:
        print("== " + cl["cell"])
        for r in cl["rows"]:
            s = "   %-22s" % r["metric"]
            for c in conds:
                if c in r:
                    s += "  %s %7.2f [%.2f..%.2f]" % (c, r[c]["med"], r[c]["min"], r[c]["max"])
            for c in conds:
                if "d" + c in r:
                    x = r["d" + c]
                    s += "  d%s %+.2f (tol %.2f) %s" % (c, x["delta"], x["tol"], "ok" if x["pass"] else "FAIL")
            print(s)
    print("== CPU in the measuring window (mean of runs, %): busy rig CPUs / other CPUs, softirq rig / other, steal, host")
    for c, p in sorted(res["cpu"].items()):
        print("   %s  rig %.1f  other %.1f  softirq %.1f / %.1f  steal %.2f  host %.1f  (%d runs)" % (
            c, p["rig"], p["other"], p["sirq_rig"], p["sirq_other"], p["steal"], p["host"], p["runs"]))
    for c, f in fails.items():
        for x in f:
            print("   failed run %s: %s" % (c, x))
    print("== verdict: " + "  ".join("%s vs A: %s" % (c, "PASS" if ok else "FAIL") for c, ok in sorted(allpass.items())))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""sumsuite.py OUTDIR [--slow REF.json THRESH] — the test-speed table (R1 speed, item 3).

For each run of suite.sh in OUTDIR (LABEL.meta): mode, wall time, exit codes,
the engine package's elapsed, test counts (pass/fail/skip, top-level tests),
every failed test (package, name, last lines of its output), the rig-lock
holders seen during the run, and the busy share of CPUs 0-5 and 6-11.
A split run's class (a) and shard files are read together; the set of
(package, test) it ran is compared with the full runs' set (coverage).

--slow REF.json THRESH: print the "package test elapsed" lines of every
top-level test that took >= THRESH s in REF.json (the class (b) list).
"""
import glob
import json
import os
import statistics
import sys


def events(path):
    try:
        f = open(path)
    except OSError:
        return
    for l in f:
        try:
            yield json.loads(l)
        except ValueError:
            continue


def read_runs(files):
    tests, pk, out = {}, {}, {}
    for p in files:
        for e in events(p):
            t, a = e.get("Test"), e.get("Action")
            if t and a == "output":
                out.setdefault((e["Package"], t.split("/")[0]), []).append(e.get("Output", "").rstrip())
            if a in ("pass", "fail", "skip"):
                if t:
                    if "/" not in t:
                        tests[(e["Package"], t)] = (a, e.get("Elapsed", 0.0))
                else:
                    pk.setdefault(e["Package"], []).append((a, e.get("Elapsed", 0.0)))
    return tests, pk, out


def busy(a, b, ids):
    try:
        A = {l.split()[0]: [int(x) for x in l.split()[1:]] for l in open(a)}
        B = {l.split()[0]: [int(x) for x in l.split()[1:]] for l in open(b)}
    except (OSError, ValueError):
        return None
    tot = idle = 0
    for i in ids:
        c = "cpu%d" % i
        d = [y - x for x, y in zip(A[c], B[c])]
        tot += sum(d[:8])
        idle += d[3] + d[4]
    return round(100 * (1 - idle / tot), 1) if tot else None


def main():
    a = sys.argv[1:]
    if a and a[0] == "--slow":
        tests, _, _ = read_runs([a[1]])
        th = float(a[2])
        for (p, t), (r, e) in sorted(tests.items(), key=lambda kv: -kv[1][1]):
            if e >= th and r != "skip":
                print(p, t, e)
        return
    d = a[0]
    rows = []
    full_sets = []
    for m in sorted(glob.glob(os.path.join(d, "*.meta")), key=os.path.getmtime):
        lab = os.path.basename(m)[:-5]
        meta = open(m).read().split("\n")
        last = [l for l in meta if l.startswith(lab + " ")]
        if not last:
            continue
        f = last[-1].split()
        mode, wall = f[1], float(f[f.index("wall") + 1])
        rc = " ".join(f[f.index("rc") + 1:f.index("head")])
        files = [os.path.join(d, lab + ".json")] if not mode.startswith("split") else \
            sorted(glob.glob(os.path.join(d, lab + "-a.json")) + glob.glob(os.path.join(d, lab + "-b*.json")))
        tests, pk, out = read_runs(files)
        eng = [e for p, v in pk.items() if p.endswith("/engine") for _, e in v]
        cnt = {"pass": 0, "fail": 0, "skip": 0}
        for r, _ in tests.values():
            cnt[r] += 1
        fails = []
        for (p, t), (r, e) in sorted(tests.items()):
            if r == "fail":
                msg = [l for l in out.get((p, t), []) if l and not l.startswith("===") and not l.startswith("---")]
                fails.append((p.split("/")[-1], t, round(e, 1), " | ".join(msg[-4:])[-400:]))
        # build failures leave a package fail with no test
        for p, v in pk.items():
            for r, _ in v:
                if r == "fail" and not any(pp == p and rr == "fail" for (pp, _), (rr, _) in tests.items()):
                    fails.append((p.split("/")[-1], "(package)", 0, "package failed with no failed test (build or timeout)"))
        holders = set()
        try:
            for l in open(os.path.join(d, lab + ".lock")):
                w = l.split()
                if len(w) > 1:
                    holders.add(w[1])
        except OSError:
            pass
        ran = {k for k, (r, _) in tests.items() if r != "skip"}
        if not mode.startswith("split") and mode != "engine":
            full_sets.append(ran)
        rows.append({"label": lab, "mode": mode, "wall": wall, "rc": rc,
                     "engine_s": round(max(eng), 1) if eng else None, "counts": cnt, "fails": fails,
                     "lock_holders": sorted(holders),
                     "busy_0_5": busy(os.path.join(d, lab + ".cpus-start"), os.path.join(d, lab + ".cpus-end"), range(0, 6)),
                     "busy_6_11": busy(os.path.join(d, lab + ".cpus-start"), os.path.join(d, lab + ".cpus-end"), range(6, 12)),
                     "ran": ran})
    ref = set.union(*full_sets) if full_sets else set()
    for r in rows:
        if r["mode"].startswith("split") and ref:
            miss = ref - r["ran"]
            r["coverage_missing"] = sorted("%s %s" % (p.split("/")[-1], t) for p, t in miss)[:20]
            r["coverage_missing_n"] = len(miss)
    by = {}
    for r in rows:
        by.setdefault(r["mode"], []).append(r)
    if "--json" in a:
        for r in rows:
            r.pop("ran")
        print(json.dumps({"runs": rows}, indent=1, ensure_ascii=False))
        return
    print("%-14s %-9s %7s %8s %-18s %5s %5s %5s  %-6s %-6s %s" % ("run", "mode", "wall s", "engine", "rc", "pass", "fail", "skip", "b0-5", "b6-11", "lock holders"))
    for r in rows:
        print("%-14s %-9s %7.1f %8s %-18s %5d %5d %5d  %-6s %-6s %s" % (
            r["label"], r["mode"], r["wall"], r["engine_s"], r["rc"], r["counts"]["pass"], r["counts"]["fail"],
            r["counts"]["skip"], r["busy_0_5"], r["busy_6_11"], ",".join(r["lock_holders"])))
        for f in r["fails"]:
            print("    FAIL %s %s (%.1f s): %s" % f)
        if r.get("coverage_missing_n"):
            print("    coverage: %d tests of the full runs not run: %s" % (r["coverage_missing_n"], ", ".join(r["coverage_missing"])))
    print("== median wall by mode (s):  " + "  ".join("%s %.0f [%.0f..%.0f] n=%d" % (
        m, statistics.median(x["wall"] for x in v), min(x["wall"] for x in v), max(x["wall"] for x in v), len(v)) for m, v in by.items()))


if __name__ == "__main__":
    main()

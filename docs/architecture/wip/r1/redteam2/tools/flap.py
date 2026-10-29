#!/usr/bin/env python3
"""flap.py RAWDIR...: the R1 red team's (second pass) look at repeated link
kills and exhaustion runs (run.sh KILLREP, PROCSTAT=1, HS2_GOSTAT=1). For
each run dir (RAWDIR/raw/<id>): the kills (when, what, sockets killed),
connections cut within 5 s of each kill by kind, the cut flows' recovery
(p50 / p95 / never), each hs2's goroutines and heap (gostat log lines) and
RSS, threads and open files (procstat) before the first kill, at the most,
and at the end, and the link sockets at the end. One markdown row a run."""
import glob
import json
import os
import re
import statistics
import sys

GS = re.compile(r"^(\d\d):(\d\d):(\d\d)(?:\.\d+)? .*gostat: goroutines (\d+) heap (\d+) sys (\d+) gc (\d+)")


def pct(v, q):
    v = sorted(v)
    return v[min(len(v) - 1, int(q * len(v)))] if v else None


def gostat(path):
    rows = []
    try:
        for l in open(path, errors="replace"):
            m = GS.match(l)
            if m:
                h, mi, s = int(m.group(1)), int(m.group(2)), int(m.group(3))
                rows.append((h * 3600 + mi * 60 + s, int(m.group(4)), int(m.group(5)) / 1e6))
    except FileNotFoundError:
        pass
    return rows


def procstat(path):
    rows = []
    try:
        for l in open(path):
            p = l.split()
            if len(p) >= 4:
                rows.append((float(p[0]), int(p[1]) / 1024.0, int(p[2]), int(p[3])))
    except FileNotFoundError:
        pass
    return rows


def first_last_max(rows, i):
    if not rows:
        return "-"
    v = [r[i] for r in rows]
    return "%.0f / %.0f / %.0f" % (v[min(3, len(v) - 1)], max(v), v[-1])


def main():
    print("| run | kills (sockets) | cut within 5 s of a kill, by kind (sum) | cut flows back p50 / p95 s, never | goroutines edge, exit (start / most / end) | heap MB edge, exit | RSS MB edge, exit | threads edge, exit | open files edge, exit | link sockets at the end |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for raw in sys.argv[1:]:
        for d in sorted(glob.glob(os.path.join(raw, "raw", "*"))):
            if not os.path.isdir(d):
                continue
            kills = []
            for k in sorted(glob.glob(d + "/kill*.json")):
                try:
                    kills.append(json.load(open(k)))
                except Exception:
                    pass
            kills.sort(key=lambda k: k.get("t_rel", 0))
            try:
                cl = json.load(open(d + "/client.json"))
            except Exception:
                cl = {}
            conns = cl.get("conns") or []
            cut = {}
            for k in kills:
                t = k.get("t_rel", 0)
                for c in conns:
                    if c.get("x", -1) >= t and c.get("x", -1) < t + 5:
                        cut[c["k"]] = cut.get(c["k"], 0) + 1
            rec = cl.get("recov") or []
            backs = [r["back"] for r in rec if r.get("back", -1) >= 0]
            never = sum(1 for r in rec if r.get("back", -1) < 0)
            ge, gx = gostat(d + "/edge.log"), gostat(d + "/exit.log")
            pe, px = procstat(d + "/procstat-edge.tsv"), procstat(d + "/procstat-exit.tsv")
            def nlinks(f):
                try:
                    return sum(1 for l in open(f) if l[:1].isdigit() or l.startswith("ESTAB"))
                except FileNotFoundError:
                    return "-"
            print("| %s | %s | %s | %s | %s; %s | %s; %s | %s; %s | %s; %s | %s; %s | %s |" % (
                os.path.basename(d),
                "%d %s (%s)" % (len(kills), kills[0].get("what") if kills else "-", sum(k.get("killed", 0) for k in kills)) if kills else "none",
                ", ".join("%s %d" % (k, v) for k, v in sorted(cut.items())) + (" (%d)" % sum(cut.values()) if cut else "-"),
                ("%.2f / %.2f, %d" % (statistics.median(backs), pct(backs, 0.95), never)) if backs else ("-, %d" % never),
                first_last_max(ge, 1), first_last_max(gx, 1),
                first_last_max(ge, 2), first_last_max(gx, 2),
                first_last_max(pe, 1), first_last_max(px, 1),
                first_last_max(pe, 2), first_last_max(px, 2),
                first_last_max(pe, 3), first_last_max(px, 3),
                nlinks(d + "/ss-edge-end.txt")))


if __name__ == "__main__":
    main()

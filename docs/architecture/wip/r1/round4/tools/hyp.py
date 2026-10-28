#!/usr/bin/env python3
"""hyp.py RES.jsonl... [--bin B]: the video hypothesis of round 4 in numbers.

For each run of a class mix with viewers: the viewers' download (main
window) and segment rate, the heavy users' download, and from the exit's
status lines (taken 1.2 s before the end) each BULK link's active users A
and delivery rate C (the budget line): a one-flow user's floor on its link
is C/A (sched.go: F_u = min(D_u, C/N_active)); the table gives the median
C/A over the BULK links next to the viewers' segment rate, and what each
link's users would have with weights 1/k (a user on k links counts 1/k on
each: the heavy users k = the links)."""
import json
import re
import statistics
import sys

args = [a for a in sys.argv[1:] if not a.startswith("--")]
want = None
if "--bin" in sys.argv:
    want = sys.argv[sys.argv.index("--bin") + 1]
    args = [a for a in args if a != want]

SCHED = re.compile(r"sched link (\d+): .* users (\d+) \(active (\d+)\)")
BUD = re.compile(r"budget link (\d+): .* delivery ([0-9.]+) Mbit/s")
CLS = re.compile(r"classes: light (\d+) / bulk (\d+)")


def med(v):
    v = [x for x in v if x is not None]
    return statistics.median(v) if v else None


def f(x, p=2):
    return "-" if x is None else ("%%.%df" % p) % x


print("| run | N | mix | viewers dn / seg Mbit/s | heavy dn | exit BULK links: active users (median, min-max) | C Mbit/s (median) | C/A median | rebuffers |")
print("|---|---|---|---|---|---|---|---|---|")
for p in args:
    for l in open(p):
        if not l.strip():
            continue
        r = json.loads(l)
        if not r.get("ok") or r.get("MIX") not in ("home", "stream_vs_dl"):
            continue
        if want and r.get("bin") != want:
            continue
        us = r.get("users") or []
        v = [u for u in us if u.get("role") == "stream"]
        h = [u for u in us if u.get("role") == "heavy"]
        vdn = med([(u.get("dn_mbit") or {}).get("main") for u in v])
        vseg = med([(u.get("stream") or {}).get("seg_mbit") for u in v])
        hdn = med([(u.get("dn_mbit") or {}).get("main") for u in h])
        lines = (r.get("status_lines") or {}).get("exit") or []
        if isinstance(lines, str):
            lines = lines.splitlines()
        act, cap = {}, {}
        cur = None
        for x in lines:
            m = SCHED.search(x)
            if m:
                cur = int(m.group(1))
                act[cur] = int(m.group(3))
                continue
            m = BUD.search(x)
            if m:
                cap[int(m.group(1))] = float(m.group(2))
        bulk = sorted(k for k in cap if k in act and act[k] > 0)
        A = [act[k] for k in bulk]
        C = [cap[k] for k in bulk]
        ca = [cap[k] / act[k] for k in bulk]
        st = (r.get("classes") or {}).get("stream") or {}
        print("| %s | %s | %s | %s / %s | %s | %s (%s-%s) | %s | %s | %s |" % (
            r["id"], r.get("N"), r.get("MIX"), f(vdn), f(vseg), f(hdn),
            f(med(A), 0), min(A) if A else "-", max(A) if A else "-", f(med(C), 1), f(med(ca)), st.get("rebuffers")))

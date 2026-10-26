#!/usr/bin/env python3
# collect.py OUT BASELINE_DIR: splits the rig outputs (OUT/A, OUT/B, OUT/C
# res.jsonl) into the baseline's blocks by the cell, runs ana.py on each, and
# copies the run logs and the host snapshots.
#   p10    RATE=100 USERS=200 lone16/one_vs_16/newheavy/tunping (priority 1)
#   s5     MIX=s5 RTT=72 RATE in {5, 25, 100, 1000} (priority 2)
#   extra  the rest: s5 at RATE=1, at RATE=50 with RTT 20 and 150, tunping TUNV6=1 (priority 3)
import json, os, shutil, subprocess, sys

out, base = sys.argv[1], sys.argv[2]
ana = os.path.join(base, "../../rig-scale/ana.py")
blocks = {"p10": [], "s5": [], "extra": []}
for r in ("A", "B", "C"):
    p = os.path.join(out, r, "res.jsonl")
    if not os.path.exists(p):
        continue
    for l in open(p):
        if not l.strip():
            continue
        x = json.loads(l)
        c = dict(t.split("=", 1) for t in x["cell"].split())
        if c["MIX"] != "s5" and c.get("TUNV6") != "1" and c["RATE"] == "100":
            blocks["p10"].append(l)
        elif c["MIX"] == "s5" and c["RTT"] == "72" and c["RATE"] in ("5", "25", "100", "1000"):
            blocks["s5"].append(l)
        else:
            blocks["extra"].append(l)
    shutil.copy(os.path.join(out, r, "run.log"), os.path.join(base, "run-%s.log" % r))
for b, ls in blocks.items():
    d = os.path.join(base, b)
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "res.jsonl"), "w") as f:
        f.writelines(ls)
    if ls:
        res = subprocess.run([sys.executable, ana, os.path.join(d, "res.jsonl")], capture_output=True, text=True)
        open(os.path.join(d, "ana.txt"), "w").write(res.stdout + res.stderr)
    print(b, len(ls), "runs")
for f in ("host.txt", "orch.log"):
    if os.path.exists(os.path.join(out, f)):
        shutil.copy(os.path.join(out, f), os.path.join(base, f))

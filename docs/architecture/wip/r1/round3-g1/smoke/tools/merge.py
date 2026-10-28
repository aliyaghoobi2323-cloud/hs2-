#!/usr/bin/env python3
"""merge.py OUT.jsonl RES.jsonl...: the res.jsonl lines of several blocks in
one file, the third-repeat labels folded into their binary (g1z -> g1,
g1wz -> g1w, g1dz/g1dy -> g1d) and each line tagged with its block (the
name of the directory it came from)."""
import json, os, sys
FOLD = {"g1z": "g1", "g1wz": "g1w", "g1dz": "g1d", "g1dy": "g1d", "g1ez": "g1e", "g1ey": "g1e"}
with open(sys.argv[1], "w") as o:
    for p in sys.argv[2:]:
        blk = os.path.basename(os.path.dirname(os.path.abspath(p)))
        for l in open(p):
            if not l.strip():
                continue
            r = json.loads(l)
            r["label0"] = r.get("bin")
            r["bin"] = FOLD.get(r.get("bin"), r.get("bin"))
            r["block"] = blk
            o.write(json.dumps(r) + "\n")

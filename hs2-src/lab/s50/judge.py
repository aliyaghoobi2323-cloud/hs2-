#!/usr/bin/env python3
"""judge.py RES... — sc runs: path c (index 2, weight 15, 30 % loss for 20 s): its lb target before the loss,
when it fell below half (t_drop), when it was back to >= 80 % of before (t_back), from the dialer's per-second status."""
import json, sys, os
for d in sys.argv[1:]:
    rows = [json.loads(l) for l in open(os.path.join(d, "d-status.jsonl")) if l.strip()]
    tg = [(r["t"], r["lb"][2][2]) for r in rows if r.get("lb") and len(r["lb"]) > 2 and r["lb"][2][2] is not None]
    if not tg: print(d, "no lb"); continue
    pre = max(v for t, v in tg[:12]) if len(tg) > 12 else max(v for _, v in tg)
    drop = next((t for t, v in tg if t > 5 and v < pre / 2), None)
    back = next((t for t, v in tg if drop is not None and t > drop + 20 and v >= 0.8 * pre), None)
    print(f"{os.path.basename(d)}: pre {pre} t_drop {drop} t_back {back} drop->back {None if back is None or drop is None else back - drop} s; end target {tg[-1][1]} (t {tg[-1][0]})")

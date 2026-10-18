# lmsum.py FILE.jsonl: per LinkManager rig run, each user's mean Mbit/s,
# when its connection ended (and why), and the degraded/closed lines
import json, sys
for f in sys.argv[1:]:
    for l in open(f):
        if not l.strip(): continue
        d = json.loads(l); r = d.get("r", d)
        for u in r["users"]:
            m = u["mbit1s"]; live = [x for x in m if x > 0]
            print(f'{r["scen"]:<20} {r["ver"]:<8} rep{r["rep"]} mean {sum(m)/max(1,len(m)):6.1f} Mbit/s  ended {u["endedAtS"]:6.1f}s {u.get("err","") or ""}')
        for x in r["lines"]:
            if "degraded" in x or "closed" in x or "drain" in x:
                print("      ", x[:230])

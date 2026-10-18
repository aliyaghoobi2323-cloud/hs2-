# cuts.py SET VER SCEN...: per run, the loss drains (cuts) of the window core inside the measurement
# window [t0+warm, t0+warm+dur] and the interactive user's p50/p99
import sys, json, datetime
sys.path.insert(0, "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z12")
from z11lib import load, basic
S = "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z12"
def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")[:26] + "+00:00").timestamp()
for d in load(sys.argv[1]):
    if d["ver"] != sys.argv[2] or d["scen"] not in sys.argv[3:]:
        continue
    e = d["env"]; w0 = ts(d["t0"]) + float(e.get("Z5_WARM_S", 6)); w1 = w0 + float(e.get("Z5_DUR_S", 24))
    up = e.get("Z5_UPL") == "1"
    side = "exit" if up else "edge"
    cuts = []
    try:
        for l in open(f"{S}/res/tr/{d['_id']}.jsonl"):
            x = json.loads(l)
            if x["side"] != side: continue
            t = ts(x["t"])
            for ev in x.get("events") or []:
                if ev.startswith("drain:loss") and w0 <= t <= w1:
                    cuts.append(round(t - w0, 1))
    except FileNotFoundError:
        pass
    b = basic(d)
    print(f"{d['_id']:40} T {b['T']:.0f} rs {b.get('rs',0):.2f} p50/p99 {b.get('p50')}/{b.get('p99')}  loss drains in window: {cuts}")

# anaH.py: the hit signal at 2 MiB windows (pinned), window-limited or not, fast or slow streams:
# per run the rate of each stream, its hit/upd and lag/upd over the measure window, and F membership.
from z11lib import *

R = load("H")
for d in sorted(R, key=lambda d: (d["scen"], d["rep"])):
    o = basic(d)
    L = trace(d["_id"], "edge")
    t0 = ts(d["t0"]) if d.get("t0") else (L[0]["_t"] if L else 0)
    w0, w1 = window(d)
    L = [x for x in L if w0 <= x["_t"] - t0 <= w1 and x["dt_s"] > 0]
    per = {}
    for x in L:
        for s in x["streams"] or []:
            if s["win"] < 1 << 20:
                continue
            p = per.setdefault(s["id"], {"upd": 0, "hit": 0, "lag": 0, "f": 0, "n": 0, "read": 0, "tmin": []})
            p["upd"] += s["upd"]; p["hit"] += s["hit"]; p["lag"] += s["lag"]; p["n"] += 1; p["f"] += s["f"]; p["read"] += s["read"]
            if s["turn_min_ms"] > 0:
                p["tmin"].append(s["turn_min_ms"])
    q = t2win(d, "qd")
    dur = w1 - w0
    e = d["env"]
    bdp = float(e["Z5_DN_MBIT"]) * 1e6 / 8 * float(e["Z5_RTT_MS"]) / 1000
    print(f"{d['_id']}: T={o['T']:.1f} (path {e['Z5_DN_MBIT']}, 2MiB={2**21/bdp:.2f} BDP) rs={o.get('rs',0):.2f} qtrue={mean(q) or 0:.1f}ms drops={o['drops']} "
          f"tbase={mean([x['t_base_ms'] for x in L]) or 0:.1f} states={sorted(set(x['state'] for x in L))}")
    for sid, p in sorted(per.items()):
        print(f"     stream {sid}: {p['read']*8/dur/1e6:6.1f} Mbit/s  hit/upd={p['hit']}/{p['upd']}={p['hit']/max(1,p['upd']):.2f} lag/upd={p['lag']/max(1,p['upd']):.2f} "
              f"F={p['f']}/{p['n']} turnmin med={pct(p['tmin'],0.5) or 0:.1f}")

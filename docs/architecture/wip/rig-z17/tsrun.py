# tsrun.py SET RID [step_s]: one run over time: rate, true queue, drops, the sender's cwnd/pacing,
# and the window core's state, B, q_turn / q_snd / q_own / q_ctrl, ooo, hit per evaluation.
import sys
from z11lib import *
name, rid = sys.argv[1], sys.argv[2]
step = float(sys.argv[3]) if len(sys.argv) > 3 else 0.5
d = [x for x in load(name) if x["_id"] == rid][0]
up = isup(d)
sx = "e" if up else "x"
t2 = d.get("t2") or {}
f1 = d.get("up1s") if up else d.get("fast1s")
L = trace(rid, "exit" if up else "edge")
t0 = ts(d["t0"]) if d.get("t0") else (L[0]["_t"] if L else 0)
e = d["env"]
bdp = float(e["Z5_DN_MBIT"]) * 1e6 / 8 * float(e["Z5_RTT_MS"]) / 1000
print(rid, {k: v for k, v in basic(d).items()}, "BDP", bdp)
q = t2.get("qu" if up else "qd") or []
dr = t2.get("dropu" if up else "dropd") or []
cw, pc, un = t2.get("cwnd" + sx) or [], t2.get("pace" + sx) or [], t2.get("unacked" + sx) or []
k = 0
n = int(len(q) / (step * 10))
for i in range(n):
    a, b = int(i * step * 10), int((i + 1) * step * 10)
    t = i * step
    rate = mean(f1[int(t):int(t) + 1]) if f1 and int(t) < len(f1) else None
    seg = [x for x in L if t <= x["_t"] - t0 < t + step]
    s = ""
    for x in seg:
        ss = [y for y in (x["streams"] or []) if y["f"]]
        hits = sum(y["hit"] for y in ss); upd = sum(y["upd"] for y in ss)
        s += (f" [{x['state'][:4]} B={x['b']/bdp:.2f} pub={x['b_pub']/bdp:.2f} qt={x['q_turn_ms']:.1f} ref={x['q_ref_ms']:.1f} "
              f"qs={x['q_snd_ms'] if x['q_snd_ok'] else -1:.1f} qo={x['q_own_ms']:.1f} qc={x['q_ctrl_ms']:.0f} ooo={100*x['ooo']:.1f} "
              f"h={hits}/{upd} rf={x['r_f']*8/1e6:.0f}" + (f" EV={[e.split()[0] for e in x['events']]}" if x.get("events") else "") + "]")
    print(f"{t:5.1f} r={rate if rate is None else round(rate)} q={max(q[a:b]) if q[a:b] else '-'} drop={dr[b-1]-dr[a] if b-1 < len(dr) else '-'} "
          f"cwnd={min(cw[a:b]) if cw[a:b] else '-'} pace={max(pc[a:b]) if pc[a:b] else '-'}" + s)

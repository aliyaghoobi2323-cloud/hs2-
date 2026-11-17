#!/usr/bin/env python3
# sum109.py [RESDIR]: per MODE x ARM heavy Mbit/s + light p99; Welch t and exact MWU (two-sided) vs arm A and vs arm B.
import json, os, sys, math, itertools, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/ab109/res"
rows = {}
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "result.json")
    if ".bad" in t or not os.path.isfile(f): continue
    try: d = json.load(open(f))
    except Exception: continue
    m = dict(x.split("=", 1) for x in open(os.path.join(R, t, "meta.txt")).read().split() if "=" in x)
    p = d.get("probe", {}); c = p.get("crowd", {}); hv = d.get("heavy", {}).get("mbps", 0)
    if not hv: continue  # crashed iperf3 = missing, not zero
    rows.setdefault((m["MD"], m["ARM"]), []).append((hv, c.get("p99_ms", 0), c.get("fail", 0),
        p.get("call", {}).get("p99_ms", 0), p.get("game", {}).get("p99_ms", 0), p.get("echo_p99_ms", 0)))
def betacf(a, b, x):
    qab, qap, qam = a + b, a + 1, a - 1; c, d = 1.0, 1 - qab * x / qap
    d = 1 / (d if abs(d) > 1e-30 else 1e-30); h = d
    for m in range(1, 300):
        m2 = 2 * m; aa = m * (b - m) * x / ((qam + m2) * (a + m2))
        d = 1 + aa * d; d = 1 / (d if abs(d) > 1e-30 else 1e-30); c = 1 + aa / c if abs(c) > 1e-30 else 1e30; h *= d * c
        aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))
        d = 1 + aa * d; d = 1 / (d if abs(d) > 1e-30 else 1e-30); c = 1 + aa / c if abs(c) > 1e-30 else 1e30; de = d * c; h *= de
        if abs(de - 1) < 1e-12: break
    return h
def ibeta(a, b, x):
    if x <= 0: return 0.0
    if x >= 1: return 1.0
    bt = math.exp(math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b) + a * math.log(x) + b * math.log(1 - x))
    return bt * betacf(a, b, x) / a if x < (a + 1) / (a + b + 2) else 1 - bt * betacf(b, a, 1 - x) / b
def welch(x, y):
    if len(x) < 2 or len(y) < 2: return float("nan")
    vx, vy = st.variance(x) / len(x), st.variance(y) / len(y)
    if vx + vy == 0: return 1.0
    t = (st.mean(x) - st.mean(y)) / math.sqrt(vx + vy)
    df = (vx + vy) ** 2 / (vx ** 2 / (len(x) - 1) + vy ** 2 / (len(y) - 1))
    return ibeta(df / 2, 0.5, df / (df + t * t))
def mwu(x, y):  # exact two-sided permutation p of U (ties: midranks)
    allv = x + y; n = len(x)
    if n == 0 or not y: return float("nan")
    srt = sorted(allv); rk = {v: (srt.index(v) + 1 + len(srt) - srt[::-1].index(v)) / 2 for v in allv}
    U = lambda idx: sum(rk[allv[i]] for i in idx) - n * (n + 1) / 2
    mu = n * len(y) / 2; obs = abs(U(range(n)) - mu); tot = hit = 0
    for c in itertools.combinations(range(len(allv)), n):
        tot += 1; hit += abs(U(c) - mu) >= obs - 1e-9
    return hit / tot
print("mode arm n | heavy med/mean (runs) | d% vs A | Welch p vs A | MWU p vs A | Welch p vs B | crowd_p99 med | fails | call | game | echo")
for (md, arm), v in sorted(rows.items()):
    h = [x[0] for x in v]; a = [x[0] for x in rows.get((md, "A"), [])]; b = [x[0] for x in rows.get((md, "B"), [])]
    med = lambda i: round(st.median(x[i] for x in v), 1)
    dA = "%+.1f" % (100 * (st.mean(h) / st.mean(a) - 1)) if a else "-"
    pa = "-" if arm == "A" else "%.3f" % welch(h, a); ma = "-" if arm == "A" else "%.3f" % mwu(h, a)
    pb = "-" if arm in ("A", "B") else "%.3f" % welch(h, b)
    print(md, arm, len(v), "|", med(0), round(st.mean(h), 1), sorted(round(x) for x in h), "|", dA, "|", pa, "|", ma, "|", pb, "|",
          med(1), "|", sum(x[2] for x in v), "|", med(3), "|", med(4), "|", med(5))

#!/usr/bin/env python3
# sum118.py [RESDIR]: per MODE x ARM (ON vs OFF, G vs ON, M* vs ON) heavy Mbit/s (mean/median/p10) + light p99 per class with Welch p vs A; quiet (q) runs kept.
import json, os, sys, math, itertools, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/dr118/res"
rows = {}
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, "result.json")
    if ".bad" in t or not os.path.isfile(f): continue
    try: d = json.load(open(f))
    except Exception: continue
    m = dict(x.split("=", 1) for x in open(os.path.join(R, t, "meta.txt")).read().split() if "=" in x)
    p = d.get("probe", {}); c = p.get("crowd", {}); hv = d.get("heavy", {}).get("mbps", 0)
    if not hv and m["MD"] != "q": continue  # crashed iperf3 = missing, not zero
    if not c.get("p99_ms"): continue
    rows.setdefault((m["MD"], m["ARM"]), []).append((hv, c.get("p99_ms", 0), c.get("fail", 0),
        p.get("call", {}).get("p99_ms", 0), p.get("game", {}).get("p99_ms", 0), p.get("echo_p99_ms", 0), float(d.get("ping_p99", 0) or 0)))
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
def mwu(x, y):  # two-sided MWU: exact permutation when C(n+m,n) <= 2e5, else normal approx with tie correction + continuity
    allv = x + y; n = len(x); m2 = len(y)
    if n == 0 or not y: return float("nan")
    srt = sorted(allv); rk = {v: (srt.index(v) + 1 + len(srt) - srt[::-1].index(v)) / 2 for v in allv}
    U = lambda idx: sum(rk[allv[i]] for i in idx) - n * (n + 1) / 2
    mu = n * m2 / 2; obs = abs(U(range(n)) - mu)
    if math.comb(n + m2, n) > 200000:
        N = n + m2; ties = sum(c ** 3 - c for c in (allv.count(v) for v in set(allv)))
        sd = math.sqrt(n * m2 / 12 * ((N + 1) - ties / (N * (N - 1))))
        return math.erfc(max(obs - 0.5, 0) / sd / math.sqrt(2)) if sd > 0 else 1.0
    tot = hit = 0
    for c in itertools.combinations(range(len(allv)), n):
        tot += 1; hit += abs(U(c) - mu) >= obs - 1e-9
    return hit / tot
def p10(h): return round(sorted(h)[max(0, int(0.1 * len(h) + 0.5) - 1)])
print("mode arm n | heavy mean med p10 | d% mean vs ref | Welch/MWU p vs ref | light p99 med/mean (diff Welch p vs ref; ref ON<-OFF, G<-ON, M<-ON): crowd call game echo ping | crowd fails")
for (md, arm), v in sorted(rows.items()):
    h = [x[0] for x in v]; REF = {"ON": "OFF", "G": "ON", "M60": "ON", "M58": "ON"}.get(arm, ""); A = rows.get((md, REF), []); a = [x[0] for x in A]
    dA = "%+.1f" % (100 * (st.mean(h) / st.mean(a) - 1)) if a and st.mean(a) else "-"
    pp = "-" if not REF or not a or md == "q" else "%.3f/%.3f" % (welch(h, a), mwu(h, a))
    lt = []
    for i, nm in ((1, "crowd"), (3, "call"), (4, "game"), (5, "echo"), (6, "ping")):
        xs = [x[i] for x in v]; ys = [x[i] for x in A]
        lt.append("%s %.1f/%.1f%s" % (nm, st.median(xs), st.mean(xs), "" if not REF or not ys else " (%+.1f p%.2f/%.2f)" % (st.mean(xs) - st.mean(ys), welch(xs, ys), mwu(xs, ys))))
    print(md, arm, len(v), "|", round(st.mean(h)), round(st.median(h)), p10(h), "|", dA, "|", pp, "|", "; ".join(lt), "|", sum(x[2] for x in v))

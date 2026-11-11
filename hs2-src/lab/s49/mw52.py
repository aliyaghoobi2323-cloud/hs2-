#!/usr/bin/env python3
# mw52.py RESDIR REGEX — groups runs NAME matching REGEX (group 1 = scenario, group 2 = arm),
# prints median [min-max] per metric, rebuild trigger lines in the Iran log, and exact two-sided
# Mann-Whitney p between arms 0 and 1 of each scenario.
import json, os, re, sys, statistics as st, itertools, glob
D, rx = sys.argv[1], re.compile(sys.argv[2])
M = [("mbps", lambda r: (r.get("heavy") or {}).get("mbps")),
     ("ping99", lambda r: r.get("ping_p99")),
     ("crowd99", lambda r: ((r.get("probe") or {}).get("crowd") or {}).get("p99_ms")),
     ("crowdfail", lambda r: ((r.get("probe") or {}).get("crowd") or {}).get("fail")),
     ("call99", lambda r: ((r.get("probe") or {}).get("call") or {}).get("p99_ms")),
     ("late%", lambda r: ((r.get("probe") or {}).get("call") or {}).get("late_pct")),
     ("game99", lambda r: ((r.get("probe") or {}).get("game") or {}).get("p99_ms"))]
g = {}
for n in sorted(os.listdir(D)):
    m = rx.match(n)
    if not m: continue
    try: r = json.load(open(f"{D}/{n}/result.json"))
    except Exception: continue
    if not r.get("probe"): continue
    trig = 0
    for f in glob.glob(f"{D}/{n}/*ir*.log"):
        trig += sum(1 for l in open(f, errors="replace") if "(HS2_MCONN_STRIPE_REBUILD)" in l)
    d = {k: f(r) for k, f in M}; d["trig"] = trig
    g.setdefault(m.group(1), {}).setdefault(m.group(2), []).append(d)
def mw(a, b):
    a = [x for x in a if x is not None]; b = [x for x in b if x is not None]
    if not a or not b: return None
    def U(x, y): return sum((1 if i > j else 0.5 if i == j else 0) for i in x for j in y)
    u = U(a, b); allv = a + b; n = len(a); cnt = 0; tot = 0
    mu = n * len(b) / 2
    import math, random
    N = len(allv)
    if math.comb(N, n) <= 20000:
        it = itertools.combinations(range(N), n)
    else:  # Monte Carlo permutation (20000 draws, seed 1)
        rnd = random.Random(1); it = (rnd.sample(range(N), n) for _ in range(20000))
    for c in it:
        s = set(c); x = [allv[i] for i in c]; y = [allv[i] for i in range(N) if i not in s]
        tot += 1; cnt += abs(U(x, y) - mu) >= abs(u - mu) - 1e-9
    return cnt / tot
def f(v):
    v = [x for x in v if x is not None]
    return f"{st.median(v):.0f} [{min(v):.0f}-{max(v):.0f}]" if v else "-"
for sc in sorted(g):
    arms = g[sc]
    print(f"== {sc}")
    for a in sorted(arms):
        L = arms[a]
        print(f"  arm {a} n={len(L)} triggers={sum(d['trig'] for d in L)} (runs with >0: {sum(1 for d in L if d['trig'])})")
        for k, _ in M: print(f"    {k:9s} {f([d[k] for d in L])}")
    ks = sorted(arms)
    if len(ks) == 2:
        print("  MW p: " + " ".join(f"{k}={mw([d[k] for d in arms[ks[0]]],[d[k] for d in arms[ks[1]]]) or 0:.2f}" for k, _ in M))

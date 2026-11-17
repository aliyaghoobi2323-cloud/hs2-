#!/usr/bin/env python3
# stat128.py A ANA_LINES...: arm A (ON|FULL) vs off (hs128)
import itertools, math, re, statistics as st, sys

def betacf(a, b, x):
    qab, qap, qam = a + b, a + 1, a - 1; c, d = 1.0, 1 - qab * x / qap
    d = 1 / (d if abs(d) > 1e-30 else 1e-30); h = d
    for m in range(1, 300):
        m2 = 2 * m; aa = m * (b - m) * x / ((qam + m2) * (a + m2))
        d = 1 + aa * d; d = 1 / (d if abs(d) > 1e-30 else 1e-30); c = 1 + aa / c if abs(1 + aa / c) > 1e-30 else 1e-30; h *= d * c
        aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))
        d = 1 + aa * d; d = 1 / (d if abs(d) > 1e-30 else 1e-30); c = 1 + aa / c if abs(1 + aa / c) > 1e-30 else 1e-30
        de = d * c; h *= de
        if abs(de - 1) < 1e-12: break
    return h

def ibeta(a, b, x):
    if x <= 0: return 0.0
    if x >= 1: return 1.0
    bt = math.exp(math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b) + a * math.log(x) + b * math.log(1 - x))
    return bt * betacf(a, b, x) / a if x < (a + 1) / (a + b + 2) else 1 - bt * betacf(b, a, 1 - x) / b

def welch(a, b):
    if len(a) < 2 or len(b) < 2: return float('nan')
    va, vb = st.variance(a) / len(a), st.variance(b) / len(b)
    if va + vb == 0: return 1.0
    t = (st.mean(a) - st.mean(b)) / math.sqrt(va + vb)
    df = (va + vb) ** 2 / (va ** 2 / (len(a) - 1) + vb ** 2 / (len(b) - 1))
    return ibeta(df / 2, 0.5, df / (df + t * t))

def mwu(a, b):
    allv = a + b; n1 = len(a)
    if not a or not b: return float('nan')
    def u(idx):
        s = 0.0
        for i in idx:
            for j in range(len(allv)):
                if j in idx: continue
                s += 1 if allv[i] > allv[j] else 0.5 if allv[i] == allv[j] else 0
        return s
    u0 = u(set(range(n1))); mu = n1 * len(b) / 2; obs = abs(u0 - mu)
    cnt = tot = 0
    for c in itertools.combinations(range(len(allv)), n1):
        tot += 1; cnt += abs(u(set(c)) - mu) >= obs - 1e-9
    return cnt / tot

rows = []
ARM = sys.argv[1]
for f in sys.argv[2:]:
    for l in open(f):
        p = l.split()
        if len(p) < 3 or p[1] not in (ARM, 'off'): continue
        p[1] = 'ON' if p[1] == ARM else 'off'
        kv = dict(re.findall(r'(\w+)=([-\d.]+)', l))
        rows.append((p[0], p[1], kv))
def cls(t):
    m = re.match(r'(qd8|qd1|qu1|qu8|xd|xu)', t); return m.group(1) if m else None
for c in ('qd8', 'qd1', 'qu1', 'qu8', 'xd', 'xu'):
    R = [r for r in rows if cls(r[0]) == c]
    if not R: continue
    print(f'== {c}')
    for k in ('mbps', 'zero', 'crowdfail', 'ping99', 'echo99', 'call99', 'game99', 'crowd99', 'cpu_ir', 'cpu_kh', 'rss_ir', 'rss_kh', 'gw', 'glive', 'cutS', 'halv'):
        A = [float(r[2][k]) for r in R if r[1] == 'ON' and k in r[2]]
        B = [float(r[2][k]) for r in R if r[1] == 'off' and k in r[2]]
        if not A and not B: continue
        f = lambda v: f'{st.median(v):8.1f}/{st.mean(v):8.1f}' if v else '       -/       -'
        print(f'  {k:9s} ON n={len(A)} med/mean {f(A)}  off n={len(B)} {f(B)}  welch p={welch(A, B):.3f} mwu p={mwu(A, B):.3f}')

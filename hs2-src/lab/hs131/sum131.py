#!/usr/bin/env python3
# sum131.py ANAFILE: per arm mean/median of the main metrics (valid runs) + Welch t vs A_off
import re, sys, statistics as st
R = {}
for l in open(sys.argv[1]):
    p = l.split()
    if len(p) < 3 or 'VOID' in l or 'no result' in l: continue
    m = dict(re.findall(r'(\w+)=([\d.]+)', l)); R.setdefault(p[1], []).append(m)
K = ['mbps', 'call99', 'crowd99', 'ping99', 'echo99', 'game99', 'slow500', 'eps', 'eplen', 'worst', 'rss_kh', 'rss_ir', 'cpu_kh']
def t(a, b):
    if len(a) < 2 or len(b) < 2: return float('nan')
    va, vb = st.variance(a), st.variance(b); d = (va/len(a) + vb/len(b)) ** .5
    return (st.mean(a) - st.mean(b)) / d if d else float('nan')
base = R.get(sys.argv[2] if len(sys.argv) > 2 else 'A_off', [])
for a in sorted(R):
    print(f"{a} n={len(R[a])}")
    for k in K:
        x = [float(m[k]) for m in R[a] if k in m]; y = [float(m[k]) for m in base if k in m]
        if x: print(f"  {k:8s} mean={st.mean(x):8.1f} med={st.median(x):8.1f} t_vs_base={t(x, y):+.2f}")

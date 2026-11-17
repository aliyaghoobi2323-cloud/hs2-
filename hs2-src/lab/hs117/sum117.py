#!/usr/bin/env python3
import sys, re, statistics as S
g = {}
for l in sys.stdin:
    if 'mbps=' not in l: continue
    t = l.split()[0]; m = dict(re.findall(r'(\w+)=([-\d.]+)', l)); g.setdefault(t[0], []).append((t, m))
for k in sorted(g):
    R = g[k]
    def med(f):
        v = [float(m[f]) for _, m in R if f in m]
        return f'{S.median(v):.0f}' if v else '-'
    print(k, f'n={len(R)} mbps med {med("mbps")}', [m.get('mbps') for _, m in R], 'zero', [m.get('zero') for _, m in R],
          'closes', [int(m.get('close_kh', 0)) + int(m.get('close_ir', 0)) for _, m in R],
          *[f'{f} {med(f)}' for f in ('ping99', 'echo99', 'call99', 'game99', 'crowd99')])

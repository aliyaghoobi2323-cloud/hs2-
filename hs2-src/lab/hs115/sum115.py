#!/usr/bin/env python3
# sum115.py: per-group medians from ana115 output on stdin (group = tag letter)
import sys, re, statistics as S
g = {}
for l in sys.stdin:
    if not l.strip(): continue
    t = l.split()[0]
    m = {k: v for k, v in re.findall(r'(\w+)=([-\d.]+)', l)}
    z = re.search(r'zero_s\(30-150\)=(\d+)', l); m['zero'] = z.group(1) if z else '-'
    g.setdefault(t[0], []).append((t, m, 'RST' if 'gapRST=1' in l else ''))
for k in sorted(g):
    rows = g[k]
    def med(f):
        v = [float(m[f]) for _, m, _ in rows if f in m]
        return f'{S.median(v):.0f}' if v else '-'
    print(k, 'n=%d' % len(rows), 'mbps med', med('mbps'), [m.get('mbps') for _, m, _ in rows], 'zero', [m['zero'] for _, m, _ in rows],
          'rst', [r for _, _, r in rows], 'ping99', med('ping99'), 'echo99', med('echo99'), 'call99', med('call99'),
          'stalls', [m.get('ackstall', '-') for _, m, _ in rows], 'veto', [m.get('stall_veto_tcp', '-') for _, m, _ in rows])

#!/usr/bin/env python3
# cmp83.py RESDIR...: per arm (meta.txt ARM, N): mean/median of light p99s, heavy total/fairness,
# LB cuts (log lines) and Mann-Whitney p of each arm vs the first arm.
import json, sys, os, glob, re, statistics as st
def load(f):
    try: return json.load(open(f))
    except Exception: return None
def jain(x):
    x = [v for v in x if v]; return (sum(x) ** 2) / (len(x) * sum(v * v for v in x)) if x else float('nan')
rows = {}
for d in sys.argv[1:]:
    if not os.path.exists(os.path.join(d, 'meta.txt')): continue
    m = open(os.path.join(d, 'meta.txt')).read(); arm = re.search(r'ARM=(\S+)', m).group(1); n = re.search(r'N=(\S+)', m).group(1)
    r = load(os.path.join(d, 'result.json')) or {}; p = r.get('probe', {})
    if not p: continue
    g = lambda k, f: (p.get(k) or {}).get(f, float('nan'))
    tot, fast = 0, []
    uf = os.path.join(d, 'hv', 'users.txt')
    if os.path.exists(uf):
        for ln in open(uf):
            a = ln.split(); j = load(os.path.join(d, 'hv', 'u%s.json' % a[0])); mb = 0
            if j and a[1] in 'fl': mb = j.get('mbps') or 0
            elif j:
                try: mb = j['end']['sum_received']['bits_per_second'] / 1e6
                except Exception: mb = 0
            tot += mb
            if a[1] == 'f': fast.append(mb)
    cuts = sum(len(re.findall(r'auto2: path \S+ cut', open(f, errors='ignore').read())) for f in glob.glob(os.path.join(d, '*-ir.log')) + glob.glob(os.path.join(d, '*-kh.log')))
    held = sum(open(f, errors='ignore').read().count('reading held') for f in glob.glob(os.path.join(d, '*.log')))
    v = dict(crowd99=g('crowd', 'p99_ms'), crowd95=g('crowd', 'p95_ms'), call99=g('call', 'p99_ms'), late=g('call', 'late_pct'), game99=g('game', 'p99_ms'),
             echo99=p.get('echo_p99_ms'), conn95=p.get('conn_p95_ms'), ping99=r.get('ping_p99'), cfail=g('crowd', 'fail'), mbps=tot, jain=jain(fast), cuts=cuts, held=held)
    rows.setdefault((n, arm), []).append(v)
try:
    from scipy.stats import mannwhitneyu
except Exception:
    mannwhitneyu = None
keys = ['crowd95', 'crowd99', 'call99', 'late', 'game99', 'echo99', 'conn95', 'ping99', 'cfail', 'mbps', 'jain', 'cuts', 'held']
for n in sorted({k[0] for k in rows}, key=int):
    arms = [k for k in rows if k[0] == n]
    base = arms[0]
    for k in arms:
        vs = rows[k]
        print('N=%s %-3s n=%d ' % (n, k[1], len(vs)) + ' '.join('%s=%.1f' % (x, st.mean([v[x] for v in vs if v[x] == v[x]] or [float('nan')])) for x in keys))
    for k in arms[1:]:
        out = []
        for x in keys:
            a = [v[x] for v in rows[base] if v[x] == v[x]]; b = [v[x] for v in rows[k] if v[x] == v[x]]
            if mannwhitneyu and len(a) > 1 and len(b) > 1 and (a != b):
                try: out.append('%s p=%.3f' % (x, mannwhitneyu(a, b).pvalue))
                except Exception: pass
        print('   %s vs %s MWU: %s' % (k[1], base[1], ', '.join(out)))
# exact permutation test of the mean difference (no scipy on the lab): per N, second arm vs first
import itertools
print('-- exact permutation p (two-sided, mean diff), arm2 vs arm1')
for n in sorted({k[0] for k in rows}, key=int):
    arms = [k for k in rows if k[0] == n]
    if len(arms) < 2: continue
    A, B = rows[arms[0]], rows[arms[1]]
    out = []
    for x in keys:
        a = [v[x] for v in A if v[x] == v[x]]; b = [v[x] for v in B if v[x] == v[x]]
        if len(a) < 2 or len(b) < 2: continue
        allv = a + b; obs = abs(st.mean(b) - st.mean(a)); c = t = 0
        for idx in itertools.combinations(range(len(allv)), len(b)):
            s = set(idx); bb = [allv[i] for i in s]; aa = [allv[i] for i in range(len(allv)) if i not in s]
            t += 1; c += abs(st.mean(bb) - st.mean(aa)) >= obs - 1e-9
        out.append('%s %+.1f%% p=%.3f' % (x, 100 * (st.mean(b) - st.mean(a)) / st.mean(a) if st.mean(a) else 0, c / t))
    print('N=%s %s vs %s: %s' % (n, arms[1][1], arms[0][1], ', '.join(out)))

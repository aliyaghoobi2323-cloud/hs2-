#!/usr/bin/env python3
# sum76.py RESDIR PREFIX...: one line per run, then per (mode, arm) means and
# Welch t / Mann-Whitney U (normal approx., two-sided) of each arm vs A.
import json, os, sys, math, statistics as st
R = sys.argv[1]; pre = sys.argv[2:]
rows = []
for t in sorted(os.listdir(R)):
    if not any(t.startswith(p) for p in pre): continue
    try:
        d = json.load(open(os.path.join(R, t, 'result.json'))); m = open(os.path.join(R, t, 'meta.txt')).read()
    except Exception: continue
    arm = m.split('ARM=')[1].split()[0]; md = m.split('MD=')[1].split()[0]
    h = d.get('heavy', {}); p = d.get('probe', {})
    if h.get('mbps', 0) <= 1: print(t, 'NO HEAVY (iperf3 failed)'); continue
    r = dict(t=t, arm=arm, md=md, mbps=h['mbps'], crowd=p['crowd']['p99_ms'], call=p['call']['p99_ms'], game=p['game']['p99_ms'],
             echo=p['echo_p99_ms'], ping=d['ping_p99'], rir=d['rss_ir_mb'], rkh=d['rss_kh_mb'])
    try:
        hj = json.load(open(os.path.join(R, t, 'heavy.json'))); r['ps'] = [round(x['sum']['bits_per_second']/1e6) for x in hj['intervals']]
    except Exception: r['ps'] = []
    rows.append(r)
    print('%-6s %-2s %s mbps=%6.1f crowd=%5.0f call=%5.0f game=%5.0f echo=%5.0f ping=%5.0f rss=%3.0f/%3.0f %s' % (t, arm, md, r['mbps'], r['crowd'], r['call'], r['game'], r['echo'], r['ping'], r['rir'], r['rkh'], r['ps']))
def welch(a, b):
    if len(a) < 2 or len(b) < 2: return float('nan')
    va, vb = st.variance(a)/len(a), st.variance(b)/len(b)
    if va + vb == 0: return float('nan')
    t = (st.mean(b)-st.mean(a))/math.sqrt(va+vb)
    df = (va+vb)**2/(va**2/(len(a)-1)+vb**2/(len(b)-1))
    # two-sided p from Student t via the regularized incomplete beta (continued fraction)
    x = df/(df+t*t)
    return betai(df/2, 0.5, x)
def betai(a, b, x):
    if x <= 0: return 0.0
    if x >= 1: return 1.0
    lb = math.lgamma(a+b)-math.lgamma(a)-math.lgamma(b)+a*math.log(x)+b*math.log(1-x)
    def cf(a, b, x):
        qab, qap, qam = a+b, a+1, a-1; c, d = 1.0, 1-qab*x/qap; d = 1/(d if abs(d) > 1e-30 else 1e-30); h = d
        for m in range(1, 200):
            m2 = 2*m; aa = m*(b-m)*x/((qam+m2)*(a+m2))
            d = 1+aa*d; d = 1/(d if abs(d) > 1e-30 else 1e-30); c = 1+aa/c; h *= d*c
            aa = -(a+m)*(qab+m)*x/((a+m2)*(qap+m2))
            d = 1+aa*d; d = 1/(d if abs(d) > 1e-30 else 1e-30); c = 1+aa/c; de = d*c; h *= de
            if abs(de-1) < 3e-12: break
        return h
    if x < (a+1)/(a+b+2): return math.exp(lb)*cf(a, b, x)/a
    return 1-math.exp(lb)*cf(b, a, 1-x)/b
def mwu(a, b):
    if not a or not b: return float('nan')
    allv = sorted([(v, 0) for v in a]+[(v, 1) for v in b]); ranks = {}
    i = 0
    while i < len(allv):
        j = i
        while j+1 < len(allv) and allv[j+1][0] == allv[i][0]: j += 1
        for k in range(i, j+1): ranks.setdefault(k, (i+j)/2+1)
        i = j+1
    rb = sum(ranks[k] for k, (v, g) in enumerate(allv) if g == 1)
    n1, n2 = len(a), len(b); u = rb-n2*(n2+1)/2; mu = n1*n2/2; sd = math.sqrt(n1*n2*(n1+n2+1)/12)
    z = (abs(u-mu)-0.5)/sd if sd else 0
    return math.erfc(z/math.sqrt(2))
print()
for md in sorted(set(r['md'] for r in rows)):
    arms = sorted(set(r['arm'] for r in rows if r['md'] == md))
    for arm in arms:
        g = [r for r in rows if r['md'] == md and r['arm'] == arm]
        f = lambda k: st.mean([x[k] for x in g])
        med = lambda k: st.median([x[k] for x in g])
        print('%s %-2s n=%d mbps mean=%6.1f med=%6.1f | p99 crowd=%5.1f call=%5.1f game=%5.1f echo=%5.1f ping=%5.1f | rss ir=%5.1f kh=%5.1f' % (md, arm, len(g), f('mbps'), med('mbps'), f('crowd'), f('call'), f('game'), f('echo'), f('ping'), f('rir'), f('rkh')))
    for x in [y for y in arms if y != 'A' and 'A' in arms]:
        for k in ['mbps', 'crowd', 'call', 'game', 'echo', 'ping', 'rir', 'rkh']:
            a = [r[k] for r in rows if r['md'] == md and r['arm'] == 'A']; b = [r[k] for r in rows if r['md'] == md and r['arm'] == x]
            print('  %s %-5s A=%7.1f %s=%7.1f d=%+6.1f%% welch p=%.3f mwu p=%.3f' % (md, k, st.mean(a), x, st.mean(b), 100*(st.mean(b)/st.mean(a)-1), welch(a, b), mwu(a, b)))

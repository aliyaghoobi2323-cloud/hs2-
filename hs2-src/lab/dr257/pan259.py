#!/usr/bin/env python3
# dr259: per-path light-link rtt (proxy for light users on each path) from pss.txt, per run.
# python3 pan259.py RUN...   (arm = run name without the trailing digit)
# For each run window, per side/path: light-link rtt p50/p99 (links that acked < 64 KB in that second and sent
# within 1 s) during download tests ("T") and outside them ("O"), plus Mbit/s per path during tests.
import sys, re, datetime, collections, statistics as st
R = '/opt/hs2lab/dr257'
def iso(s): return datetime.datetime.strptime(s, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=datetime.timezone.utc).timestamp()
runs = {}
for n in sys.argv[1:]:
    d = R + '/' + n
    a = iso(re.search(r'start=(\S+)', open(d + '/env.txt').read()).group(1))
    b = iso(open(d + '/DONE').read().split()[1])
    W = []
    for l in open(d + '/tests.txt'):
        m = re.search(r'start=([\d.]+) dir=(\S+)', l)
        if m: W.append((float(m.group(1)), float(m.group(1)) + 13, m.group(2)))
    runs[n] = (a, b, W)
prev = {}
acc = {n: collections.defaultdict(list) for n in runs}
byt = {n: collections.defaultdict(int) for n in runs}
for l in open(R + '/pss.txt'):
    p = l.split()
    if len(p) < 7: continue
    t = float(p[0]); side, path, key = p[1], p[2], p[1] + p[3]
    rtt, ak, ls = float(p[4]), int(p[5]), int(p[6])
    pv = prev.get(key); prev[key] = (t, ak)
    if not pv or t - pv[0] > 2.5 or ak < pv[1]: continue
    dlt = ak - pv[1]
    for n, (a, b, W) in runs.items():
        if not (a + 60 <= t <= b - 20): continue
        w = next((x for x in W if x[0] + 2 <= t <= x[1]), None)
        tag = ('T' + ('d' if w[2] == '-R' else 'u')) if w else 'O'
        if dlt < 65536 and ls < 1000: acc[n][(side, path, tag)].append(rtt)
        if w: byt[n][(side, path, tag)] += dlt
def q(v, x): v = sorted(v); return v[min(len(v) - 1, int(len(v) * x))] if v else float('nan')
arm = collections.defaultdict(lambda: collections.defaultdict(list))
for n in runs:
    A = acc[n]; nt = collections.Counter()
    for (a, b, W) in [runs[n]]:
        for w in W: nt['Td' if w[2] == '-R' else 'Tu'] += 11
    print('==', n)
    for side in ('kh', 'ir'):
        for tag in ('Td', 'Tu', 'O'):
            rows = []
            for path in sorted({k[1] for k in A if k[0] == side}):
                v = A.get((side, path, tag), [])
                if len(v) < 20: continue
                mb = byt[n].get((side, path, tag), 0) * 8 / 1e6 / nt[tag] if tag != 'O' and nt[tag] else 0
                rows.append('%s:%.0f/%.0f%s' % (path, q(v, .5), q(v, .99), '/%dM' % mb if mb else ''))
                arm[n.rstrip('0123456789')][(side, path, tag)].append(q(v, .99))
            if rows: print('  %s %-2s p50/p99[/Mbit] %s' % (side, tag, ' '.join(rows)))
print('ARM per-path light rtt p99, mean over runs; per side/tag: path:p99 ... | mean-over-paths max-path')
for side in ('kh', 'ir'):
    for tag in ('Td', 'Tu', 'O'):
        for an in sorted(arm):
            ps = sorted({k[1] for k in arm[an] if k[0] == side and k[2] == tag})
            v = [(p, st.mean(arm[an][(side, p, tag)])) for p in ps]
            if not v: continue
            print('  %s %-2s %-3s %s | mean %.1f max %.1f runs=%d' % (side, tag, an, ' '.join('%s:%.0f' % x for x in v),
                  st.mean(x[1] for x in v), max(x[1] for x in v), len(arm[an][(side, ps[0], tag)])))

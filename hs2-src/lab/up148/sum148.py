import json, os, sys, re, glob
R = '/opt/hs2lab/up148/res'
rows = []
for d in sorted(glob.glob(R + '/*')):
    t = os.path.basename(d)
    try:
        env = open(d + '/env.txt').read().split()
        r = json.loads(open(d + '/result.json').read() or '{}')
    except Exception as e:
        print(t, 'ERR', e); continue
    h = {}
    try:
        h = json.load(open(d + '/heavy.json'))
    except Exception:
        pass
    up = per = None
    if h.get('end'):
        up = h['end']['sum_sent']['bits_per_second'] / 1e6
        per = sorted(s['sender']['bits_per_second'] / 1e6 for s in h['end']['streams'])
    lb = open(d + '/ir-lb.log', errors='replace').read() if os.path.exists(d + '/ir-lb.log') else ''
    upl = [l for l in lb.splitlines() if 'upload' in l]
    cut = [l for l in upl if ' cut ' in l]
    w = re.findall(r'path (\S+) weight ([0-9.]+)', lb)
    minw = {}
    for p, x in w:
        minw[p] = min(minw.get(p, 1), float(x))
    kh = open(d + '/kh-lb.log', errors='replace').read() if os.path.exists(d + '/kh-lb.log') else ''
    khcut = len(re.findall(r' cut \(', kh))
    pan = open(d + '/panics.txt').read().split() if os.path.exists(d + '/panics.txt') else []
    g = lambda k: r.get(k, '-')
    print('%-7s %-14s up %6s Mb/s streams %s | ping p50 %s p99 %s | call %s game %s | echo p50/p99 %s | conn_p95 %s | upcuts %d minw %s | khcuts %d | pan %s' % (
        t, ' '.join(env[:2]), '%.0f' % up if up else '-', [round(x) for x in per] if per else '-',
        g('ping_p50'), g('ping_p99'), r.get('probe',{}).get('call',{}).get('p99_ms','-'), r.get('probe',{}).get('game',{}).get('p99_ms','-'),
        '%s/%s' % (r.get('probe',{}).get('echo_p50_ms','-'), r.get('probe',{}).get('echo_p99_ms','-')), r.get('probe',{}).get('conn_p95_ms','-'), len(cut), {k: v for k, v in minw.items()}, khcut, ' '.join(pan)))

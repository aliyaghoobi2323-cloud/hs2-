#!/usr/bin/env python3
# rs271 g2an.py RUN... : Q1 hot-fallback A/B summary
import json, sys, os, re, gzip, statistics
L = '/opt/hs2lab/rs271/'
for n in sys.argv[1:]:
    d = L + n + '/'
    if not os.path.exists(d + 'DONE'):
        print(n, 'not done'); continue
    env = open(d + 'env.txt').read()
    fb = re.search(r'HS2_DL273_HOTFB=(\d+)', env).group(1)
    lab = re.search(r'lab=(\d+)', env).group(1)
    tv = []
    for ln in open(d + 'tests.txt'):
        m = re.search(r'test_mbit=(\d+)', ln)
        tv.append(int(m.group(1)) if m else 0)
    rows = [l.split() for l in open(d + 'min.txt')]
    p99 = [float(r[3]) for r in rows if r[3] != '-' and int(r[1]) > 50000]
    fails = sum(int(r[4]) for r in rows)
    try:
        p = json.load(open(d + 'result.json'))['probe']
        ep, cp, gp = p.get('echo_p99_ms'), (p.get('call') or {}).get('p99_ms'), (p.get('game') or {}).get('p99_ms')
        cr = p.get('crowd') or {}
    except Exception as e:
        ep = cp = gp = None; cr = {}
    last = None
    with gzip.open(d + 'logs/ir.full.log.gz', 'rt', errors='replace') as f:
        for ln in f:
            if 'dlprim2[' in ln: last = ln
    m = re.search(r'picks=(\d+) t0=(\d+) t1=(\d+) t2=(\d+)(?: t3=(\d+))? hotref=(\d+) scanned=(\d+) none=(\d+)', last or '')
    if m:
        pk, t0, t1, t2, t3, hr, sc, no = [int(x or 0) for x in m.groups()]
        ps = '%d/%d (%.0f%%) t0/t1/t2/t3 %d/%d/%d/%d hotref %d/%d scanned (%.0f%%)' % (pk, pk + no, 100.0 * pk / max(1, pk + no), t0, t1, t2, t3, hr, sc, 100.0 * hr / max(1, sc))
    else:
        ps = 'no dlprim2 line'
    pan = [open(d + s + '.panics').read().strip() for s in ('ir', 'kh')]
    print('%s FB=%s slot=%s DL median %s (n=%d %s) | crowd p99/min %s fails %d | echo99 %s call99 %s game99 %s | pick %s | panics %s' % (
        n, fb, lab, statistics.median(tv) if tv else None, len(tv), tv, [round(x) for x in p99], fails, ep, cp, gp, ps, pan))
    if cr: print('   crowd', {k: cr[k] for k in list(cr)[:8]})

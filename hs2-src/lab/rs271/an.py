#!/usr/bin/env python3
# rs271 an.py RUN... : long-lived conn outcome after the kharej restart, crowd fails per second, kh counters
import json, sys, os, re, gzip
L = '/opt/hs2lab/rs271/'
for n in sys.argv[1:]:
    d = L + n + '/'
    if not os.path.exists(d + 'll.json'):
        print(n, 'no ll.json'); continue
    j = json.load(open(d + 'll.json'))
    rt = None
    if os.path.exists(d + 'restart.txt'):
        for ln in open(d + 'restart.txt'):
            if 'SIGTERM' in ln: rt = float(ln.split()[0])
    cs = [c for c in j['conns'].values() if c.get('open') != 'fail']
    pre = [c for c in cs if c.get('fail') and rt and c['fail'] < rt]
    kinds = {}
    dl = []
    for c in cs:
        if c.get('fail') and (rt is None or c['fail'] >= rt):
            kinds[c['kind']] = kinds.get(c['kind'], 0) + 1
            if rt: dl.append(c['fail'] - rt)
    hang = [c for c in cs if not c.get('fail') and c.get('tmo')]
    alive = [c for c in cs if not c.get('fail') and not c.get('tmo')]
    dl.sort()
    q = lambda p: round(dl[min(len(dl) - 1, int(p * len(dl)))], 1) if dl else None
    tm = sorted(c['tmo'] - rt for c in hang) if rt else []
    print('%s conns %d openfail %d pre-restart-fail %d | after restart: %s detect s p50 %s p90 %s p99 %s max %s | hung (echo lost, no RST/EOF till end) %d (first unanswered send +%s..+%s s) | alive-ok %d | rtt p50 %s p99 %s max %s' % (
        n, len(cs), j['open_fail'], len(pre), kinds, q(.5), q(.9), q(.99), q(1), len(hang),
        round(tm[0], 1) if tm else '-', round(tm[-1], 1) if tm else '-', len(alive), j['rtt_p50'], j['rtt_p99'], j['rtt_max']))
    # crowd: fails.txt / min.txt per second rows: "sec ..." ; print fails around restart
    t0 = int(open(d + 't0').read())
    try:
        fs = [float(x) for x in open(d + 'fails.txt') if x.strip()]
        ra = (rt - t0) if rt else None
        if ra:
            w = [x for x in fs if ra - 2 <= x <= ra + 90]
            print('   crowd fails total %d, in restart window %d, first %+.1f last %+.1f s, 95%% done by %+.1f s, outside window %d' % (
                len(fs), len(w), (w[0] - ra) if w else 0, (w[-1] - ra) if w else 0, (sorted(w)[int(.95 * (len(w) - 1))] - ra) if w else 0, len(fs) - len(w)))
        else:
            print('   crowd fails total %d' % len(fs))
        rows = [l.split() for l in open(d + 'min.txt')]
        print('   per-min crowd [min n p50 p99 fails call99 game99]:', ' | '.join(' '.join(r) for r in rows))
    except Exception as e:
        print('   crowd', e)
    for s in ('kh', 'ir'):
        f = d + s + '.rescue.txt'
        if os.path.exists(f):
            ls = open(f).read().splitlines()
            jm = [re.search(r'joinmissed (\d+)', l) for l in ls]
            jr = [re.search(r'jm271 rst (\d+) limited (\d+)', l) for l in ls]
            print('   %s rescue lines %d last joinmissed %s jm271 %s' % (s, len(ls), jm[-1].group(1) if ls and jm[-1] else '-', jr[-1].groups() if ls and jr[-1] else '-'))
    for s in ('ir', 'kh'):
        print('   %s panics %s' % (s, open(d + s + '.panics').read().strip() if os.path.exists(d + s + '.panics') else '?'))

#!/usr/bin/env python3
# bh93sum.py RESDIR...: blackhole detection on the dialing side (kh = exit) and the edge (ir):
# low = first factor <= 0.5 of an event path after the event, cut = first cut, out = every event path cut
# (IP event: both paths; single path: = cut), rec = event-path recoveries while still blackholed (kh),
# silF = silent readings/cuts on NON-event paths (both sides; must be 0), false = fc90 false cuts (both
# sides, not the lossy pair, not the event), light classes + heavy as fc90sum.
import sys, os, re, glob, json, collections, statistics as st
def sec(t):
    h, m, s = t.split(':'); return int(h) * 3600 + int(m) * 60 + float(s)
def norm(p):
    a, b = p.split('>')
    return (a, b) if a.startswith('192.168.5') and a != '192.168.50.2' else (b, a)
GEN = ('192.168.51.1', '192.168.49.2')
rows = collections.defaultdict(list)
INF = float('inf')
for d in sys.argv[1:]:
    mf = os.path.join(d, 'meta.txt')
    if not os.path.exists(mf): continue
    m = open(mf).read(); arm = re.search(r'ARM=(\S+)', m).group(1); sc = re.search(r'SC=(\S+)', m).group(1)
    ev = None
    if os.path.exists(os.path.join(d, 'ev.txt')):
        for l in open(os.path.join(d, 'ev.txt')):
            f = l.split()
            if 'drop' in l and '0.025' not in l:
                ir = f[-3].split('>')[1]; kh = None if 'ALL>' in l else '192.168.49.2'
                ev = (sec(f[0]), ir, kh)
    isev = lambda k: ev is not None and k[0] == ev[1] and (ev[2] is None or k[1] == ev[2])
    need = (1 if ev and ev[2] else 2)
    res = {}
    for side, pat in (('ir', '*-ir.log'), ('kh', '*-kh.log')):
        fs = glob.glob(os.path.join(d, pat))
        r = dict(low=None, cut=None, out=None, rec=0, silF=0, silE=0, false=0, pre=False)
        if not fs: res[side] = r; continue
        cs = {}; evcut = set(); snap = False; lastcut = {}
        for l in open(fs[0], errors='replace'):
            mt = re.match(r'(\d\d:\d\d:\d\d\.\d+) ', l)
            if not mt: continue
            t = sec(mt.group(1))
            if ev and not snap and t >= ev[0]:
                snap = True
                pc = [k for k, c in cs.items() if c and isev(k)]
                if len(pc) >= need: r['pre'] = True
            after = ev is not None and t >= ev[0]
            ms = re.search(r'lb auto2: path (\S+) silent:', l)
            if ms:
                if isev(norm(ms.group(1))) and after: r['silE'] += 1
                else: r['silF'] += 1
            mr = re.search(r'lb auto2: path (\S+) recovering', l)
            if mr:
                k = norm(mr.group(1)); cs[k] = False
                if isev(k) and after: r['rec'] += 1; evcut.discard(k)
            if 'weight factor' in l and after:
                for a, o, n in re.findall(r'(\d+\.\d+\.\d+\.\d+>\d+\.\d+\.\d+\.\d+) ([\d.]+)→([\d.]+)', l):
                    if isev(norm(a)) and float(n) <= 0.5 and r['low'] is None: r['low'] = t - ev[0]
            mm = re.search(r'lb auto2: path (\S+) cut \((.*?)\)', l)
            if not mm: continue
            k = norm(mm.group(1)); cs[k] = True
            if isev(k): lastcut[k] = t
            if 'silent' in mm.group(2) and not (isev(k) and after): r['silF'] += 1
            if k == GEN: continue
            if isev(k) and after:
                if r['cut'] is None: r['cut'] = t - ev[0]
                evcut.add(k)
                if len(evcut) >= need and r['out'] is None: r['out'] = t - ev[0]
                continue
            r['false'] += 1
        if ev:
            # held = the cut that lasts to the end (a cut before the event counts 0); a blackholed path
            # back in service at the end = miss
            evp = [kk for kk in set(cs) | set(lastcut) if isev(kk)]
            hs = [max(0.0, lastcut[kk] - ev[0]) if cs.get(kk) else INF for kk in evp]
            r['held'] = (min(hs) if need == 1 else max(hs)) if len(hs) >= need else INF
            for x in ('low', 'cut', 'out'):
                if r[x] is None: r[x] = 0.0 if r['pre'] else INF
                if r['low'] is not None and r['cut'] is not None and r['cut'] < r['low']: r['low'] = r['cut']
        res[side] = r
    try: jr = json.load(open(os.path.join(d, 'result.json')))
    except Exception: jr = {}
    p = jr.get('probe', {}) if isinstance(jr.get('probe'), dict) else {}
    cl = lambda k: p.get(k, {}) if isinstance(p.get(k), dict) else {}
    cp99 = cl('crowd').get('p99_ms'); cf = cl('crowd').get('fail'); call = cl('call').get('p99_ms'); game = cl('game').get('p99_ms')
    tot = 0.0
    uf = os.path.join(d, 'hv', 'users.txt')
    if os.path.exists(uf):
        for ln in open(uf):
            a = ln.split()
            if len(a) < 6: continue
            try: j = json.load(open(os.path.join(d, 'hv', 'u%s.json' % a[0])))
            except Exception: continue
            if a[1] in 'fl': tot += j.get('mbps') or 0
            else:
                try: tot += j['end']['sum_received']['bits_per_second'] / 1e6
                except Exception: pass
    k, i = res['kh'], res['ir']
    f1 = lambda v: '-' if v is None else ('miss' if v == INF else f"{v:.1f}")
    print(f"{os.path.basename(d):9} {arm} {sc:3} kh low={f1(k['low'])} cut={f1(k['cut'])} out={f1(k['out'])} held={f1(k.get('held'))} rec={k['rec']}{' PRE' if k['pre'] else ''} | ir cut={f1(i['cut'])} out={f1(i['out'])} | silF={k['silF']}+{i['silF']} silE={k['silE']} false={k['false']}+{i['false']} | crowd99={cp99} fail={cf} call99={call} game99={game} mbps={tot:.0f}")
    rows[(sc, arm)].append((k, i, cp99, cf, call, game, tot))
def md(v):
    v = [x for x in v if isinstance(x, (int, float))]
    return f"{st.median(v):.1f}" if v else '-'
def dd(v):
    fin = [x for x in v if x != INF and x is not None]
    return f"{md(fin)}/{max(fin):.1f} miss={sum(1 for x in v if x == INF)}" if fin else f"- miss={sum(1 for x in v if x == INF)}"
print('-- per scenario/arm (kh = dialing side decides; medians; det med/max)')
print('sc  arm n | kh low | kh cut | kh out | kh held(out of service to the end) | ir out | rec/run | silF total | false kh+ir mean | crowd99 crowdfail(mean) call99 game99 | Mbit/s')
for key in sorted(rows):
    v = rows[key]
    ks = [x[0] for x in v]; irs = [x[1] for x in v]
    print(f"{key[0]:3} {key[1]} {len(v)} | {dd([x['low'] for x in ks])} | {dd([x['cut'] for x in ks])} | {dd([x['out'] for x in ks])} | {dd([x.get('held') for x in ks])} | {dd([x['out'] for x in irs])} | {st.mean(x['rec'] for x in ks):.2f} | {sum(x['silF'] for x in ks) + sum(x['silF'] for x in irs)} | {st.mean(x['false'] + y['false'] for x, y in zip(ks, irs)):.2f} | {md([x[2] for x in v])} {st.mean([x[3] for x in v if isinstance(x[3], (int, float))] or [0]):.0f} {md([x[4] for x in v])} {md([x[5] for x in v])} | {st.mean(x[6] for x in v):.0f}")

#!/usr/bin/env python3
# sum100.py RESDIR...: bh100 (from sum98.py; readm/rec also count 'is back' (cooled-path probe up); lnk = event-path links in lbtime at -5 s/clear+30/+60/+110)
# bh98 long-blackhole A/B (event 25 s, clear 145 s). Per run, dialing side (kh):
# cut = first event-path cut after the event (s), readm = event-path "recovering" lines while still
# blackholed (must be 0), held = every event path cut from its cut to the clear, rec = s from the clear
# until every event path is recovering again, silF = silent readings/cuts on NON-event paths (kh+ir),
# hold = "still silent" lines; crowd failures (probe fail_t) in [ev,+30), [+30,+120), [clear,+25).
import sys, os, re, glob, json, collections, datetime, random, statistics as st
def sec(t):
    h, m, s = t.split(':'); return int(h) * 3600 + int(m) * 60 + float(s)
def norm(p):
    a, b = p.split('>')
    return (a, b) if a.startswith('192.168.5') and a != '192.168.50.2' else (b, a)
rows = collections.defaultdict(list)
for d in sys.argv[1:]:
    mf = os.path.join(d, 'meta.txt')
    if not os.path.exists(mf): continue
    m = open(mf).read(); arm = re.search(r'ARM=(\S+)', m).group(1); sc = re.search(r'SC=(\S+)', m).group(1)
    ev = clr = None
    for l in open(os.path.join(d, 'ev.txt')):
        f = l.split()
        if '0.025' in l: continue
        ir = f[-3].split('>')[1]; kh = None if 'ALL>' in l else '192.168.49.2'
        if ' undrop ' in l: clr = sec(f[0])
        elif ' drop ' in l: ev = (sec(f[0]), ir, kh)
    if not ev: continue
    isev = lambda k: k[0] == ev[1] and (ev[2] is None or k[1] == ev[2])
    need = 1 if ev[2] else 2
    cut = None; pre_done = False; pcs = {}; readm = 0; cs = {}; recs = {}; hold = 0; silF = 0; heldbad = False
    for side in ('kh', 'ir'):
        fs = glob.glob(os.path.join(d, '*-%s.log' % side))
        if not fs: continue
        for l in open(fs[0], errors='replace'):
            mt = re.match(r'(\d\d:\d\d:\d\d\.\d+) ', l)
            if not mt: continue
            t = sec(mt.group(1)); after = t >= ev[0]
            if side == 'kh' and after and cut is None and not pre_done:
                pre_done = True
                if sum(1 for k, v in pcs.items() if v and isev(k)) >= need: cut = 0.0; cs.update({k: True for k, v in pcs.items() if v and isev(k)})
            if side == 'kh' and not after:
                mp = re.search(r'lb auto2: path (\S+) (cut \(|recovering)', l)
                if mp: pcs[norm(mp.group(1))] = mp.group(2).startswith('cut')
            ms = re.search(r'lb auto2: path (\S+) (silent:|cut \(silent)', l)
            if ms and not (isev(norm(ms.group(1))) and after): silF += 1
            if side != 'kh': continue
            if 'still silent' in l and after: hold += 1
            mm = re.search(r'lb(?: auto2)?: path (\S+) (?:cut \(|cooled)', l)  # bh100: a cooled path is out too
            if mm and isev(norm(mm.group(1))) and after:
                cs[norm(mm.group(1))] = True
                if cut is None: cut = t - ev[0]
            mr = re.search(r'lb(?: auto2)?: path (\S+) (?:recovering|is back)', l)
            if mr and isev(norm(mr.group(1))):
                k = norm(mr.group(1))
                if after and (clr is None or t < clr):
                    if cs.get(k): readm += 1
                    cs[k] = False
                elif clr is not None and t >= clr and k not in recs:
                    recs[k] = t - clr
    held = cut is not None and readm == 0 and sum(1 for v in cs.values() if v) >= need
    rec = max(recs.values()) if len(recs) >= need else None
    try: cr = json.load(open(os.path.join(d, 'result.json')))['probe']['crowd']
    except Exception: print(os.path.basename(d), 'no result'); continue
    ft = cr.get('fail_t') or []
    day = datetime.datetime.fromtimestamp(os.path.getmtime(os.path.join(d, 'ev.txt')), datetime.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0).timestamp()
    e0 = day + ev[0]; c0 = day + (clr or ev[0] + 120)
    w1 = sum(1 for x in ft if e0 <= x < e0 + 30); w2 = sum(1 for x in ft if e0 + 30 <= x < e0 + 120); w3 = sum(1 for x in ft if c0 <= x < c0 + 30)
    fmt = lambda v: '-' if v is None else '%.1f' % v
    print(f"{os.path.basename(d):9} {arm} {sc} cut={fmt(cut)} readm={readm} held={'Y' if held else 'N'} rec={fmt(rec)} hold={hold} silF={silF} | fails ev+30={w1} +30..120={w2} clr+30={w3} total={cr['fail']} p99={cr.get('p99_ms')}")
    lk = '-'
    lf = os.path.join(d, 'lbtime.txt')
    if os.path.exists(lf):
        tl = {}
        for l in open(lf):
            f = l.split()
            if not f or not f[0].isdigit(): continue
            n = 0
            for x in f[1:]:
                p, v = x.split('=', 1)
                if isev(norm(p)): n += int(v.split('/')[0])
            tl[int(f[0])] = n
        lk = '/'.join(str(tl.get(x, '?')) for x in (20, 175, 205, 255))
    print('   lnk', lk)
    rows[(sc, arm)].append(dict(w1=w1, w2=w2, w3=w3, cut=cut, readm=readm, held=held, rec=rec, silF=silF))
def perm(a, b, n=20000):
    r = random.Random(1); obs = abs(st.mean(a) - st.mean(b)); pool = a + b; k = 0
    for _ in range(n):
        r.shuffle(pool)
        if abs(st.mean(pool[:len(a)]) - st.mean(pool[len(a):])) >= obs - 1e-9: k += 1
    return k / n
print('-- per scenario')
for sc in sorted({k[0] for k in rows}):
    for arm in sorted({k[1] for k in rows if k[0] == sc}):
        v = rows[(sc, arm)]
        cuts = [x['cut'] for x in v if x['cut'] is not None]; recs = [x['rec'] for x in v if x['rec'] is not None]
        print(f"{sc} {arm} n={len(v)} ev+30 mean={st.mean(x['w1'] for x in v):.0f} [{' '.join(str(x['w1']) for x in v)}]  +30..120 mean={st.mean(x['w2'] for x in v):.0f} [{' '.join(str(x['w2']) for x in v)}]  clr+30 mean={st.mean(x['w3'] for x in v):.0f} | cut med/max {fmt2(cuts) if False else ('%.1f/%.1f' % (st.median(cuts), max(cuts)) if cuts else '-')} miss={len(v)-len(cuts)} held={sum(x['held'] for x in v)}/{len(v)} readm={sum(x['readm'] for x in v)} rec med/max {('%.1f/%.1f' % (st.median(recs), max(recs))) if recs else '-'} silF={sum(x['silF'] for x in v)}")
    a, b = rows.get((sc, 'S0'), []), rows.get((sc, 'S1'), [])
    if len(a) >= 2 and len(b) >= 2:
        print(f"{sc} p(ev+30)={perm([x['w1'] for x in a], [x['w1'] for x in b]):.3f} p(+30..120)={perm([x['w2'] for x in a], [x['w2'] for x in b]):.3f}")

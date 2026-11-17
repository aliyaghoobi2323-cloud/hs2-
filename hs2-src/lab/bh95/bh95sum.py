#!/usr/bin/env python3
# bh95sum.py RESDIR...: per-EVENT crowd failures (probe fail_t) in [ev, ev+30 s] and the 20 s before
# (rate/s), detection on the dialing side (kh) as bh93sum (low/cut/held, PRE = event path already cut
# at the event), silent false triggers; per scenario S0 vs S1 with a two-sided permutation p (means).
import sys, os, re, glob, json, collections, random, statistics as st
sys.argv, dirs = sys.argv[:1], sys.argv[1:]
def sec(t):
    h, m, s = t.split(':'); return int(h) * 3600 + int(m) * 60 + float(s)
import datetime
def unix_of(day_unix, hms):  # ev.txt is local-time %T on the lab (UTC)
    return day_unix + sec(hms)
rows = collections.defaultdict(list)
exec(open('/opt/hs2lab/bh93/bh93sum.py').read().split('rows = collections.defaultdict(list)')[0])  # sec/norm/GEN
for d in dirs:
    mf = os.path.join(d, 'meta.txt')
    if not os.path.exists(mf): continue
    m = open(mf).read(); arm = re.search(r'ARM=(\S+)', m).group(1); sc = re.search(r'SC=(\S+)', m).group(1)
    try: jr = json.load(open(os.path.join(d, 'result.json'))); cr = jr['probe']['crowd']
    except Exception: print(os.path.basename(d), 'no result'); continue
    ft = cr.get('fail_t') or []
    evl = [l for l in open(os.path.join(d, 'ev.txt')) if ' drop 1' in l]
    if not evl: continue
    hms = evl[0].split()[0]
    t0 = min(ft) if ft else None
    # unix time of the event: midnight UTC of the run day + %T (lab clock is UTC)
    st_mtime = os.path.getmtime(os.path.join(d, 'ev.txt'))
    day = datetime.datetime.fromtimestamp(st_mtime, datetime.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0).timestamp()
    ev = day + sec(hms)
    win = sum(1 for x in ft if ev <= x < ev + 30)
    pre = sum(1 for x in ft if ev - 20 <= x < ev)
    sec1 = collections.Counter(int(x - ev) for x in ft if ev <= x < ev + 30)
    peak = max(sec1.values()) if sec1 else 0
    # detection via bh93sum on this dir
    out = os.popen(f"python3 /opt/hs2lab/bh93/bh93sum.py {d} | head -1").read().strip()
    det = re.search(r'kh low=(\S+) cut=(\S+) out=\S+ held=(\S+) rec=\d+( PRE)?', out)
    sil = re.search(r'silF=(\d+)\+(\d+)', out)
    print(f"{os.path.basename(d):9} {arm} {sc} fails[ev,ev+30]={win:4d} pre20s={pre:3d} peak/s={peak:3d} total={cr['fail']:4d} open={cr['open']} p99={cr['p99_ms']} | kh low={det.group(1)} cut={det.group(2)} held={det.group(3)}{det.group(4) or ''} silF={sil.group(1)}+{sil.group(2)}")
    rows[(sc, arm)].append(dict(win=win, pre=pre, held=det.group(3), cut=det.group(2), low=det.group(1), PRE=bool(det.group(4)), silF=int(sil.group(1)) + int(sil.group(2))))
def perm(a, b, n=20000):
    r = random.Random(1); obs = abs(st.mean(a) - st.mean(b)); pool = a + b; k = 0
    for _ in range(n):
        r.shuffle(pool)
        if abs(st.mean(pool[:len(a)]) - st.mean(pool[len(a):])) >= obs - 1e-9: k += 1
    return k / n
def fl(v):
    f = [float(x) for x in v if x not in ('miss', '-')]
    return f"{st.median(f):.1f}/{max(f):.1f}" if f else '-', sum(1 for x in v if x == 'miss')
print('-- per scenario: fails in [ev, ev+30 s] mean / median (each run), S0 vs S1 permutation p (means); kh low/cut/held med/max miss')
for sc in sorted({k[0] for k in rows}):
    a, b = rows.get((sc, 'S0'), []), rows.get((sc, 'S1'), [])
    for arm, v in (('S0', a), ('S1', b)):
        if not v: continue
        w = [x['win'] for x in v]
        print(f"{sc} {arm} n={len(v)} fails mean={st.mean(w):.0f} med={st.median(w):.0f} [{' '.join(map(str, w))}] pre20 mean={st.mean(x['pre'] for x in v):.0f} | low {fl([x['low'] for x in v])} cut {fl([x['cut'] for x in v])} held {fl([x['held'] for x in v])} PRE={sum(x['PRE'] for x in v)} silF={sum(x['silF'] for x in v)}")
    if len(a) >= 2 and len(b) >= 2:
        print(f"{sc} p(S0 vs S1 fails)={perm([x['win'] for x in a], [x['win'] for x in b]):.3f}")

#!/usr/bin/env python3
# sum106.py RESDIR...: fl106 per run: crowd fails total / per flap cycle (dark start .. +50 s), crowd p99 per 5 s
# bucket (median and worst bucket), call/game p99, heavy down Mbit/s, event-path (49.2>53.1) cuts/recoveries/penalties
# (kh log), event-path links at each clean phase (lbtime), lower<->ok flips of the 0.8 % paths (lbtime weights).
import sys, os, re, glob, json, datetime, statistics as st
EP = ('192.168.49.2', '192.168.53.1')
def sec(t):
    h, m, s = t.split(':'); return int(h) * 3600 + int(m) * 60 + float(s)
def q(v, p):
    v = sorted(v); return v[min(len(v) - 1, int(len(v) * p))] if v else float('nan')
for d in sys.argv[1:]:
    try:
        meta = open(os.path.join(d, 'meta.txt')).read()
    except OSError: continue
    arm = re.search(r'ARM=(\S+)', meta).group(1); sc = re.search(r'SC=(\S+)', meta).group(1)
    t0w = float(open(os.path.join(d, 'probe_t0')).read())
    day = datetime.datetime.fromtimestamp(t0w); mid = datetime.datetime(day.year, day.month, day.day).timestamp()
    p0 = t0w - mid  # probe start, s since local midnight
    darks = []
    for l in open(os.path.join(d, 'ev.txt')):
        if '53.1 drop 1' in l and '49.2' in l: darks.append(sec(l.split()[0]) - p0)
    fails, cs, ks, gs = [], [], [], []
    for l in open(os.path.join(d, 'samples.txt')):
        f = l.split()
        if len(f) < 3: continue
        s = float(f[1])
        if f[0] == 'F': fails.append(s)
        elif f[0] == 'c': cs.append((s, float(f[2])))
        elif f[0] == 'k': ks.append(float(f[2]))
        elif f[0] == 'g': gs.append(float(f[2]))
    B = {}
    for s, v in cs: B.setdefault(int(s // 5), []).append(v)
    bp = [q(v, .99) for k, v in sorted(B.items()) if len(v) > 50]
    r = json.load(open(os.path.join(d, 'result.json')))
    hv = r.get('heavy_mbps', r.get('heavy', {}))
    if isinstance(hv, dict): hv = hv.get('mbps', hv.get('down_mbps', '-'))
    pc = ['%d' % sum(1 for x in fails if a <= x < a + 50) for a in darks]
    cut = rec = pen = 0; tl = []; cls = {}; flips = 0
    kh = glob.glob(os.path.join(d, '*-kh.log'))
    for l in open(kh[0], errors='replace') if kh else []:
        for p, rs in re.findall(r"(192\.168\.\d+\.\d+>192\.168\.54\.1) [\d.]+→[\d.]+ \((\w+)", l) if "weight factor" in l else []:
            c = "lower" if rs == "lower" else "ok" if rs in ("ok", "recovering") else None
            if c and p in cls and cls[p] != c: flips += 1
            if c: cls[p] = c
        if "lb auto2: path 192.168.49.2>192.168.53.1" not in l and 'lb auto2: path 192.168.53.1>192.168.49.2' not in l: continue
        mt = re.match(r'(\d\d:\d\d:\d\d\.\d+) ', l); t = sec(mt.group(1)) - p0 if mt else -1
        if ' cut (' in l: cut += 1; tl.append('c%.0f' % t)
        elif 'back from flap' in l or 'recover' in l and 'recdbg' not in l: rec += 1; tl.append('r%.0f' % t)
        if 'flapping' in l: pen += 1; tl.append('P%.0f' % t if 'kept out' in l else 'p%.0f' % t)
    print('%-6s %-3s %-3s fails %4d cyc [%s] crowd p99 med5s %.0f worst5s %.0f all %.0f call p99 %.0f game p99 %.0f hv %s ev-path cut %d rec %d pen %d flips54/10min %.1f %s' % (
        os.path.basename(d), arm, sc, len(fails), ' '.join(pc), q(bp, .5), max(bp) if bp else 0, q([v for _, v in cs], .99),
        q(ks, .99), q(gs, .99), hv, cut, rec, pen, flips * 600.0 / max(1, (max(s for s, _ in cs) if cs else 600)), ' '.join(tl[:16])))

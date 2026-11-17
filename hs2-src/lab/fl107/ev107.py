#!/usr/bin/env python3
# ev107.py RESDIR...: event path 49.2>53.1 links over time (lbtime "t path=served/target"), its last kh log
# lines (cut / recovering / back), fails in dark phases 2-5 (flap) and after the clear (bP).
import sys, os, re
EPK = '192.168.49.2>192.168.53.1'
for d in sys.argv[1:]:
    try: meta = open(os.path.join(d, 'meta.txt')).read()
    except OSError: continue
    sc = re.search(r'SC=(\S+)', meta).group(1)
    pts = []
    for l in open(os.path.join(d, 'lbtime.txt')):
        f = l.split(); m = re.search(re.escape(EPK) + r'=(\d+)/(\d+)', l)
        if f and m: pts.append((float(f[0]), int(m.group(1)), int(m.group(2))))
    sel = [p for p in pts if (sc == 'bP' and 120 <= p[0] <= 240) or (sc == 'F' and 0 <= p[0])]
    s = ' '.join('%d:%d' % (t, a) for t, a, b in sel[::2])
    rec = ''
    if sc == 'bP':
        r = [t for t, a, b in pts if t > 150 and a >= 5]
        rec = 'links>=5 at %s' % ('%.0f' % r[0] if r else 'never')
    F = [float(l.split()[1]) for l in open(os.path.join(d, 'samples.txt')) if l.startswith('F ')]
    post = sum(1 for x in F if 150 <= x < 240) if sc == 'bP' else ''
    print(os.path.basename(d), sc, rec, 'fails>=150s', post, '|', s[:300])

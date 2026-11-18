#!/usr/bin/env python3
# ul268s: data-dark one path at a time (Iran->kharej data packets, IP len >= 128, dropped on the
# path's mid ingress; ACKs/keepalives pass). usage: pathdark.py LAB_ID LOG T0FILE START EVERY DUR PATHS TT
import subprocess, sys, time, random
ID, LOG, T0F = sys.argv[1], sys.argv[2], sys.argv[3]
START, EVERY, DUR, NP, TT = (float(x) for x in sys.argv[4:9])
NP = int(NP)
while True:
    try: t0 = float(open(T0F).read()); break
    except Exception: time.sleep(0.5)
out = open(LOG, 'a', buffering=1)
order = list(range(NP)); random.Random(268).shuffle(order)
i = 0; t = START
def dev(p): return f'xa{ID}' + ('' if p == 0 else chr(97 + p))
m = ['protocol','ip','basic','match','cmp(u16 at 2 layer network gt 127)']
while t + DUR < TT - 20:
    while time.time() - t0 < t: time.sleep(0.1)
    p = order[i % NP]; i += 1; d = dev(p)
    r = subprocess.run(['ip','netns','exec','mid'+ID,'tc','filter','add','dev',d,'ingress','prio','1']+m+['action','drop'],capture_output=True,text=True)
    print(f"{time.time()-t0:.1f} dark path={p} dev={d} rc={r.returncode} {r.stderr.strip()}", file=out)
    time.sleep(DUR)
    subprocess.run(['ip','netns','exec','mid'+ID,'tc','filter','del','dev',d,'ingress','prio','1'])
    print(f"{time.time()-t0:.1f} light path={p}", file=out)
    t += EVERY

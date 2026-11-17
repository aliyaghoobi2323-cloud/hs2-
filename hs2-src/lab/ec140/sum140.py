#!/usr/bin/env python3
# sum140.py RESDIR: echo p50/p99/max + heavy + crowd/game p99 per run, medians per mode x arm.
import json, os, sys, statistics as st
R = sys.argv[1]; rows = {}
for t in sorted(os.listdir(R)):
    f = os.path.join(R, t, 'result.json')
    try: j = json.loads(open(f).read()); p = j['probe']
    except Exception: continue
    md, arm = open(os.path.join(R, t, 'meta.txt')).read().split()[2:0:-1]
    md, arm = md.split('=')[1], arm.split('=')[1]
    hv = (j.get("heavy") or {}).get("mbps", 0) or 0
    e = (p.get('echo_p50_ms', 0), p.get('echo_p99_ms', 0), p.get('echo_max_ms', 0), p.get('echo_n', 0))
    print(f"{t:10s} {arm:3s} {md:3s} echo n {e[3]:4d} p50 {e[0]:6.1f} p99 {e[1]:6.1f} max {e[2]:6.1f}  game p99 {(p.get("game") or {}).get("p99_ms",0):6.1f}  heavy {hv:7.1f}")
    rows.setdefault((md, arm), []).append(e)
for (md, arm), v in sorted(rows.items()):
    p50 = st.median(x[0] for x in v); p99 = [x[1] for x in v]
    print(f"{md:3s} {arm:3s} n={len(v):2d} echo p50 med {p50:6.1f}  p99 med {st.median(p99):6.1f} mean {st.mean(p99):6.1f}  max med {st.median(x[2] for x in v):6.1f}  [{' '.join('%.0f'%x for x in p99)}]")
# permutation p (two-sided, mean difference) of echo p99 and p95 vs arm H, per mode
import random
def perm(a, b, n=20000):
    d0 = abs(st.mean(a) - st.mean(b)); ab = a + b; k = 0; rnd = random.Random(1)
    for _ in range(n):
        rnd.shuffle(ab)
        if abs(st.mean(ab[:len(a)]) - st.mean(ab[len(a):])) >= d0 - 1e-9: k += 1
    return k / n
P95 = {}
for t in sorted(os.listdir(R)):
    try:
        j = json.loads(open(os.path.join(R, t, 'result.json')).read()); m = open(os.path.join(R, t, 'meta.txt')).read().split()
        P95.setdefault((m[2].split('=')[1], m[1].split('=')[1]), []).append((j['probe']['echo_p99_ms'], j['probe']['echo_p95_ms'], j.get('ping_p99', 0)))
    except Exception: pass
for (md, arm), v in sorted(P95.items()):
    h = P95.get((md, 'H'))
    if arm == 'H' or not h or len(v) < 2 or len(h) < 2: continue
    a99, h99 = [x[0] for x in v], [x[0] for x in h]; a95, h95 = [x[1] for x in v], [x[1] for x in h]
    print(f"{md:3s} {arm:3s} vs H: echo p99 mean {st.mean(a99):6.1f} vs {st.mean(h99):6.1f} p={perm(a99,h99):.2f}  p95 mean {st.mean(a95):6.1f} vs {st.mean(h95):6.1f} p={perm(a95,h95):.2f}  hs0-ping p99 {st.mean(x[2] for x in v):6.1f} vs {st.mean(x[2] for x in h):6.1f}")

# Moved flows: what they do after the one-time move (edge capture).
import gzip, json, sys, collections
fl = collections.defaultdict(dict)
for line in gzip.open(sys.argv[1], 'rt'):
    d = json.loads(line); k = round(d['t'] / 100)
    u, n = fl[d['f']].get(k, (0, 0)); fl[d['f']][k] = (u + d['up'], n + d['dn'])
KI = 1024
def moved_at(up, dn):
    best = None
    for x in (dn, up):
        s = 0; run = 0
        for i, v in enumerate(x):
            s += v
            if i >= 20: s -= x[i - 20]
            run = run + 1 if v >= 4 * KI else 0
            if s >= 64 * KI and run >= 3:
                best = i if best is None else min(best, i); break
    return best
n = 0; long_after = 0; light_frac = []; light_ticks_active = 0; active_ticks = 0
quiet10 = 0
for f, m in fl.items():
    a, b = min(m), max(m)
    up = [0] * (b - a + 1); dn = [0] * (b - a + 1)
    for k, (u, v) in m.items(): up[k - a] += u; dn[k - a] += v
    mv = moved_at(up, dn)
    if mv is None: continue
    n += 1
    rest = len(up) - mv - 1
    if rest >= 600: long_after += 1
    # after the move: ticks where the flow moves bytes, and how many of those sit in a 2 s window under 64 KiB both ways (light by volume)
    s_u = s_d = 0; lt = at = 0
    run_quiet = 0; had_q10 = False
    for i in range(len(up)):
        s_u += up[i]; s_d += dn[i]
        if i >= 20: s_u -= up[i - 20]; s_d -= dn[i - 20]
        if i <= mv: continue
        light = s_u < 64 * KI and s_d < 64 * KI
        run_quiet = run_quiet + 1 if light else 0
        if run_quiet >= 100: had_q10 = True
        if up[i] + dn[i] > 0:
            at += 1; lt += light
    active_ticks += at; light_ticks_active += lt
    if had_q10: quiet10 += 1
print('moved flows', n, 'with >=60 s of life after the move', long_after)
print('after the move: active ticks that are light by volume %.1f%%' % (100 * light_ticks_active / max(1, active_ticks)))
print('moved flows that later stay light >=10 s in a row', quiet10)

# Classify flows of the edge capture (c1_replay) into rough user kinds and
# apply the r1-r2j move rule and the C1 class rule.
import gzip, json, sys, collections, statistics as st
path = sys.argv[1]
fl = collections.defaultdict(dict)   # f -> {tick: (up, dn)}
for line in gzip.open(path, 'rt'):
    d = json.loads(line)
    k = round(d['t'] / 100)
    u, n = fl[d['f']].get(k, (0, 0))
    fl[d['f']][k] = (u + d['up'], n + d['dn'])
KI = 1024
def series(m):
    a, b = min(m), max(m)
    up = [0] * (b - a + 1); dn = [0] * (b - a + 1)
    for k, (u, n) in m.items():
        up[k - a] += u; dn[k - a] += n
    return a, up, dn
def moved(x):  # edge rx rule (download): 64 KiB over 20 ticks and >=4 KiB in 3 ticks in a row
    s = 0; run = 0
    for i, v in enumerate(x):
        s += v
        if i >= 20: s -= x[i - 20]
        run = run + 1 if v >= 4 * KI else 0
        if s >= 64 * KI and run >= 3:
            return i
    return None
def c1(x):  # C1 verdict per tick, sticky bulk until <8 KiB/2s
    cls = 'L'; out = []; s = 0
    for i, v in enumerate(x):
        s += v
        if i >= 20: s -= x[i - 20]
        b200 = v + (x[i - 1] if i else 0)
        if cls == 'B' and s >= 8 * KI: pass
        elif s < 64 * KI and b200 < 16 * KI: cls = 'L'
        else: cls = 'B'
        out.append(cls)
    return out
kinds = collections.Counter(); kbytes = collections.Counter(); kmoved = collections.Counter()
tot = 0; nflow = 0; mv_n = 0; mv_bytes = 0; bulk_ticks_steady = 0
ex = collections.defaultdict(list)
for f, m in fl.items():
    a, up, dn = series(m)
    nflow += 1
    B = sum(up) + sum(dn); tot += B
    dur = len(dn) / 10
    mi = moved(dn); mu = moved(up)
    mv = mi is not None or mu is not None
    # 1 s bins
    sec = [sum(dn[i:i+10]) + sum(up[i:i+10]) for i in range(0, len(dn), 10)]
    secd = [sum(dn[i:i+10]) for i in range(0, len(dn), 10)]
    active = [s for s in sec if s > 0]
    # kind heuristics
    kind = 'other'
    if B < 64 * KI and dur < 30:
        kind = 'short-small'          # tiny: web/API/DNS-ish
    elif dur >= 20:
        # steady: >=80% of seconds active, 1 s rate cv < 0.6, mean 0.1..4 Mbit/s
        busy = [s for s in secd]
        act = sum(1 for s in sec if s > 2 * KI) / max(1, len(sec))
        mean = st.mean(sec) * 8 / 1e6
        cv = (st.pstdev(sec) / st.mean(sec)) if st.mean(sec) > 0 else 9
        # on/off bursts: count bursts >=256 KiB separated by >=1.5 s quiet (<4 KiB/s)
        bursts = 0; inb = False; acc = 0; quiet = 0
        for s in secd:
            if s >= 32 * KI:
                if not inb and quiet >= 2: pass
                inb = True; acc += s; quiet = 0
            else:
                if inb and acc >= 256 * KI:
                    bursts += 1
                if s < 4 * KI: quiet += 1
                inb = False if s < 4 * KI else inb
                if not inb: acc = 0
        if mean < 0.25 and act >= 0.6:
            kind = 'thin-steady (game/voice/keepalive-chat)'
        elif 0.25 <= mean <= 4 and act >= 0.8 and cv < 0.6:
            kind = 'steady 0.25-4 Mbit/s (call/live)'
        elif bursts >= 3:
            kind = 'on/off bursts (VOD-like)'
        elif mean > 4 or max(sec) * 8 / 1e6 > 8:
            kind = 'bulk (download/upload)'
        else:
            kind = 'long-other'
    else:
        if B >= 1 << 20:
            kind = 'short-bulk (>=1 MiB, <20 s)'
        else:
            kind = 'short-medium (web page/object)'
    kinds[kind] += 1; kbytes[kind] += B
    if mv: kmoved[kind] += 1; mv_n += 1; mv_bytes += B
    if len(ex[kind]) < 3 and dur >= 20:
        ex[kind].append((round(dur), round(B / 1e6, 2), [round(s * 8 / 1e6, 2) for s in sec[:20]]))
print('flows', nflow, 'bytes MB', round(tot / 1e6, 1))
print('moved (r1-r2j rule) flows', mv_n, 'bytes share', round(mv_bytes / tot, 3))
for k, v in kinds.most_common():
    print('%-40s flows %5d (%4.1f%%) bytes %7.1f MB (%4.1f%%) moved %d' % (k, v, 100 * v / nflow, kbytes[k] / 1e6, 100 * kbytes[k] / tot, kmoved[k]))
for k, v in ex.items():
    print('ex', k, v)

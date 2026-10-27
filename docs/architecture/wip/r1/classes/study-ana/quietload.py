# What would ride the QUIET link under the r1-r2j move rule: bytes and
# record-rate estimate per second, per direction (edge capture).
import gzip, json, sys, collections
path = sys.argv[1]
fl = collections.defaultdict(dict)
for line in gzip.open(path, 'rt'):
    d = json.loads(line)
    k = round(d['t'] / 100)
    u, n = fl[d['f']].get(k, (0, 0))
    fl[d['f']][k] = (u + d['up'], n + d['dn'])
KI = 1024
REC = 991  # mean shaped record payload (bulk HTTPS sampler)
qb = collections.Counter(); qr = collections.Counter()   # per (sec, dir)
allb = collections.Counter()
T0 = min(min(m) for m in fl.values()); T1 = max(max(m) for m in fl.values())
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
never = 0
for f, m in fl.items():
    a, b = min(m), max(m)
    up = [0] * (b - a + 1); dn = [0] * (b - a + 1)
    for k, (u, n) in m.items():
        up[k - a] += u; dn[k - a] += n
    mv = moved_at(up, dn)
    end = len(up) if mv is None else mv + 1
    if mv is None: never += 1
    for i in range(len(up)):
        sec = (a + i - T0) // 10
        allb[(sec, 'up')] += up[i]; allb[(sec, 'dn')] += dn[i]
        if i < end:
            for dname, v in (('up', up[i]), ('dn', dn[i])):
                if v:
                    qb[(sec, dname)] += v
                    qr[(sec, dname)] += max(1, -(-v // REC))
nsec = (T1 - T0) // 10 + 1
for dname in ('up', 'dn'):
    b = sorted(qb[(s, dname)] for s in range(nsec)); r = sorted(qr[(s, dname)] for s in range(nsec))
    tb = sum(allb[(s, dname)] for s in range(nsec))
    print('%s: QUIET bytes/s p50 %.0f KB p99 %.0f KB (%.1f%% of %s bytes); records/s p50 %d p90 %d p99 %d max %d' % (
        dname, b[nsec // 2] / 1e3, b[int(nsec * .99)] / 1e3, 100 * sum(b) / tb, dname, r[nsec // 2], r[int(nsec * .9)], r[int(nsec * .99)], r[-1]))
print('flows never moved', never, 'of', len(fl), 'seconds', nsec)

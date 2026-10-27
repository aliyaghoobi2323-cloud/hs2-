# Bursts in the edge capture: runs of download ticks >= 4 KiB (gaps <= 3 ticks).
import gzip, json, sys, collections
path = sys.argv[1]
fl = collections.defaultdict(dict)
for line in gzip.open(path, 'rt'):
    d = json.loads(line)
    k = round(d['t'] / 100)
    u, n = fl[d['f']].get(k, (0, 0))
    fl[d['f']][k] = (u + d['up'], n + d['dn'])
KI = 1024
bursts = []   # (flow, start, ticks, bytes, gap_before_s)
tot_dn = 0; tot_up = 0
upb = []
for f, m in fl.items():
    a, b = min(m), max(m)
    dn = [0] * (b - a + 1); up = [0] * (b - a + 1)
    for k, (u, n) in m.items():
        dn[k - a] += n; up[k - a] += u
    tot_dn += sum(dn); tot_up += sum(up)
    for arr, store in ((dn, bursts), (up, upb)):
        i = 0; last_end = None
        while i < len(arr):
            if arr[i] >= 4 * KI:
                j = i; gap = 0; s = 0; e = i
                while j < len(arr) and gap <= 3:
                    if arr[j] >= 4 * KI:
                        gap = 0; e = j
                    else:
                        gap += 1
                    s += arr[j]; j += 1
                g = None if last_end is None else (i - last_end) / 10
                store.append((f, i, e - i + 1, s, g))
                last_end = e; i = e + 1
            else:
                i += 1
def bucket(sz):
    for lim, name in ((64 * KI, '<64K'), (256 * KI, '64-256K'), (1 << 20, '256K-1M'), (4 << 20, '1-4M'), (16 << 20, '4-16M'), (1 << 62, '>=16M')):
        if sz < lim: return name
print('dn total MB', round(tot_dn / 1e6), 'up total MB', round(tot_up / 1e6))
for name, bb, tot in (('download', bursts, tot_dn), ('upload', upb, tot_up)):
    c = collections.Counter(); cb = collections.Counter(); dur = collections.defaultdict(list); rate = collections.defaultdict(list)
    for (f, s, t, sz, g) in bb:
        k = bucket(sz); c[k] += 1; cb[k] += sz; dur[k].append(t / 10); rate[k].append(sz * 8 / (t / 10) / 1e6)
    print('==', name, 'bursts', len(bb))
    for k in ('<64K', '64-256K', '256K-1M', '1-4M', '4-16M', '>=16M'):
        if c[k]:
            ds = sorted(dur[k]); rs = sorted(rate[k])
            print('  %-8s n %5d bytes %5.1f%%  dur p50 %5.1fs p90 %6.1fs  rate p50 %5.2f p90 %5.2f Mbit/s' % (
                k, c[k], 100 * cb[k] / tot, ds[len(ds) // 2], ds[int(len(ds) * .9)], rs[len(rs) // 2], rs[int(len(rs) * .9)]))
# repeated segment pattern: flows with >=3 download bursts of 256K-16M each separated by gaps 1..15 s
per = collections.defaultdict(list)
for b in bursts: per[b[0]].append(b)
vod_flows = 0; vod_bytes = 0; gaps = []; segs = []
for f, bl in per.items():
    seg = [b for b in bl if 256 * KI <= b[3] < 16 << 20 and b[4] is not None and 1 <= b[4] <= 15]
    if len(seg) >= 3:
        vod_flows += 1; vod_bytes += sum(b[3] for b in bl)
        gaps += [b[4] for b in seg]; segs += [b[3] for b in seg]
gaps.sort(); segs.sort()
print('segment-like flows', vod_flows, 'their dn share', round(vod_bytes / tot_dn, 3))
if gaps:
    print('  gap p10/p50/p90 s', gaps[len(gaps) // 10], gaps[len(gaps) // 2], gaps[int(len(gaps) * .9)])
    print('  seg p10/p50/p90 KiB', segs[len(segs) // 10] // KI, segs[len(segs) // 2] // KI, segs[int(len(segs) * .9)] // KI)
# concurrency
act = collections.Counter()
for f, m in fl.items():
    for k, (u, n) in m.items():
        if u + n > 0: act[k // 10] += 0  # placeholder
secs = collections.defaultdict(set)
for f, m in fl.items():
    for k, (u, n) in m.items():
        if u + n >= 4 * KI: secs[k // 10].add(f)
v = sorted(len(s) for s in secs.values())
print('flows moving >=4KiB in a second: p50', v[len(v) // 2], 'p90', v[int(len(v) * .9)], 'max', v[-1])

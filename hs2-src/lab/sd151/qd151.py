import re, sys, glob, os
# per run: share of Iran->kharej (upload) packets on path 1 (dev xb<ID>b) from 40 s after setup to the end
for d in sorted(glob.glob('/opt/hs2lab/sd151/res/*')):
    f = d + '/qd.txt'
    if not os.path.exists(f): continue
    samples = []; cur = None
    dev = None
    for l in open(f, errors='replace'):
        if l.startswith('T '):
            cur = {'t': float(l.split()[1])}; samples.append(cur); continue
        m = re.match(r'qdisc netem \S+ dev (\S+) ', l)
        if m: dev = m.group(1); continue
        m = re.match(r'\s*Sent (\d+) bytes (\d+) pkt \(dropped (\d+)', l)
        if m and dev and cur is not None and re.match(r'xb\d+[a-z]?$', dev):
            cur[dev] = (int(m.group(2)), int(m.group(3))); dev = None
    samples = [s for s in samples if len(s) > 3]
    if len(samples) < 5: continue
    t0 = samples[0]['t']
    a = next(s for s in samples if s['t'] >= t0 + 40); b = samples[-1]
    devs = sorted(k for k in b if k != 't')
    tot = sum(b[k][0] - a.get(k, (0, 0))[0] for k in devs)
    p1 = [k for k in devs if k.endswith('b')][0]
    s1 = b[p1][0] - a[p1][0]; d1 = b[p1][1] - a[p1][1]
    env = open(d + '/env.txt').read().split()
    print('%-6s %-8s %-6s upload pkts %9d  path1 share %5.1f%%  path1 drops %5.2f%%' % (os.path.basename(d), env[0], env[1], tot, 100.0 * s1 / max(tot, 1), 100.0 * d1 / max(s1 + d1, 1)))

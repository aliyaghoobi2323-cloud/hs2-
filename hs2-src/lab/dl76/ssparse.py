#!/usr/bin/env python3
# ssparse.py ss.txt: per second, the exit's busy link sockets (>1 MB acked in
# the second): count, Mbit, median cwnd/unacked, app_limited share, notsent,
# and the share of the second each spent busy / rwnd-limited / sndbuf-limited.
import re, sys, statistics as st
blocks = []; cur = None
lines = open(sys.argv[1]).read().split('\n')
i = 0
while i < len(lines):
    l = lines[i]
    if l.startswith('@T '):
        cur = (float(l.split()[1]), {}); blocks.append(cur); i += 1; continue
    if cur is not None and l.strip() and not l.startswith('\t') and not l.startswith(' '):
        parts = l.split()
        key = parts[2] + '>' + parts[3] if len(parts) > 3 else l
        info = lines[i + 1] if i + 1 < len(lines) else ''
        cur[1][key] = info; i += 2; continue
    i += 1
def g(rx, s, d=0.0):
    m = re.search(rx, s); return float(m.group(1)) if m else d
prev = None
for t, socks in blocks:
    if prev:
        dt = t - prev[0]; rows = []
        for k, info in socks.items():
            p = prev[1].get(k)
            if not p: continue
            da = g(r'bytes_acked:(\d+)', info) - g(r'bytes_acked:(\d+)', p)
            if da < 1e6: continue
            rows.append(dict(mb=da*8/dt/1e6, cwnd=g(r'cwnd:(\d+)', info), unacked=g(r'unacked:(\d+)', info),
                app='app_limited' in info, notsent=g(r'notsent:(\d+)', info),
                busy=(g(r'busy:(\d+)ms', info)-g(r'busy:(\d+)ms', p))/dt/1000,
                rwnd=(g(r'rwnd_limited:(\d+)ms', info)-g(r'rwnd_limited:(\d+)ms', p))/dt/1000,
                sndbuf=(g(r'sndbuf_limited:(\d+)ms', info)-g(r'sndbuf_limited:(\d+)ms', p))/dt/1000,
                rtt=g(r' rtt:([\d.]+)', info), dr=g(r'delivery_rate (\d+)', info)/1e6,
                retr=g(r'retrans:\d+/(\d+)', info)-g(r'retrans:\d+/(\d+)', p)))
        if rows:
            f = lambda k: st.median([r[k] for r in rows])
            print('%5.1f n=%2d Mbit=%6.0f cwnd=%4.0f unacked=%4.0f app=%3.0f%% notsent=%7.0f busy=%3.0f%% rwnd=%3.0f%% sndbuf=%3.0f%% rtt=%5.1f dr=%5.0f retr=%d' % (
                t - blocks[0][0], len(rows), sum(r['mb'] for r in rows), f('cwnd'), f('unacked'),
                100*sum(r['app'] for r in rows)/len(rows), st.mean([r['notsent'] for r in rows]),
                100*f('busy'), 100*f('rwnd'), 100*f('sndbuf'), f('rtt'), f('dr'), sum(r['retr'] for r in rows)))
    prev = (t, socks)

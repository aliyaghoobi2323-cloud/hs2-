import json, re, sys, os, gzip
R = '/opt/hs2lab/ul268s/runs'
def last(lines, pat):
    for l in reversed(lines):
        if pat in l: return l
    return ''
for n in sys.argv[1:]:
    d = os.path.join(R, n)
    if not os.path.exists(d + '/DONE'): print(n, 'not done'); continue
    env = open(d + '/env.txt').read(); edge = re.search(r'edge=(\w+)', env).group(1)
    ts, zero = [], 0
    for l in open(d + '/tests.txt'):
        m = re.search(r'test_mbit=(\d+) \[(.*)\]', l)
        if not m: continue
        ts.append(int(m.group(1))); iv = [int(x) for x in m.group(2).split(',')]
        zero += sum(1 for x in iv[1:] if x < 1)
    try:
        j = json.load(open(d + '/result.json')); p = j['probe']
        lat = 'crowdF %d crowd99 %.0f icmp99 %.0f echo99 %.0f call99 %.0f callmax %.0f' % (p['crowd']['fail'], p['crowd']['p99_ms'], j.get('ping_p99', -1), p['echo_p99_ms'], p['call']['p99_ms'], p['call']['max_ms'])
        rss = 'rss ir %s kh %s' % (j.get('rss_ir_mb'), j.get('rss_kh_mb'))
    except Exception as e: lat, rss = 'probe? %s' % e, ''
    # cpu from mon.txt
    cpu = '-'; rssmax = 0
    try:
        L = [x.split() for x in open(d + '/mon.txt')]
        by = {}
        for x in L:
            by.setdefault(x[1], []).append(x)
            rssmax = max(rssmax, int(x[4]) // 1024)
        pid = max(by, key=lambda k: len(by[k])); a, b = by[pid][0], by[pid][-1]
        cpu = '%.1f%%' % (100 * ((int(b[2]) + int(b[3])) - (int(a[2]) + int(a[3]))) / 100.0 / (float(b[0]) - float(a[0])))
    except Exception as e: cpu = 'cpu? %s' % e
    lines = gzip.open(d + '/logs/ir.full.log.gz', 'rt', errors='replace').read().splitlines()
    u = last(lines, 'ul268: dark'); sc = last(lines, 'ul268 scale:')
    U = dict(re.findall(r'(dark|ticks|noalt|stripe|rescued|nocov|callskip) (\d+)', u.split('ul268: ', 1)[1])) if u else {}
    S = dict(re.findall(r'(visits|conns|held|maxus|full|outqno|dcskip|dcyield) (\d+)', sc)) if sc else {}
    mins = 140 / 60.0
    pm = lambda D, k: '%.1f' % (int(D.get(k, 0)) / mins)
    ul = 'ul268/min dark %s rescued %s nocov %s callskip %s stripe %s | visits %s conns %s held %s maxus %s full %s dcskip %s dcyield %s outqno %s' % (
        pm(U, 'dark'), pm(U, 'rescued'), pm(U, 'nocov'), pm(U, 'callskip'), pm(U, 'stripe'), pm(S, 'visits'), pm(S, 'conns'), pm(S, 'held'), S.get('maxus', '-'), pm(S, 'full'), pm(S, 'dcskip'), pm(S, 'dcyield'), pm(S, 'outqno'))
    pan = open(d + '/kh.panics').read().strip() + '/' + open(d + '/ir.panics').read().strip()
    nd = sum(1 for l in open(d + '/dark.txt') if ' dark ' in l) if os.path.exists(d + '/dark.txt') else 0
    print('%s edge=%s faults=%d up tests=%s zero_s=%d | %s | cpu_ir %s rssmax_ir %d %s | %s | panics %s' % (n, edge, nd, ts, zero, lat, cpu, rssmax, rss, ul, pan))

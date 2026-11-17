import sys, re, os, glob, statistics as st
# an145.py RUNSDIR: one row per run + per-arm means
R = sys.argv[1]
def nums(line, keys):
    out = {}
    for k in keys:
        m = re.search(r'\b' + k + r' (\d+)', line)
        out[k] = int(m.group(1)) if m else 0
    return out
rows = []
for d in sorted(glob.glob(R + '/*/')):
    tag = os.path.basename(d.rstrip('/'))
    if not os.path.exists(d + 'DONE'): continue
    ir = open(d + 'resc-ir.txt').read() if os.path.exists(d + 'resc-ir.txt') else ''
    kh = open(d + 'resc-kh.txt').read() if os.path.exists(d + 'resc-kh.txt') else ''
    raw_ir = ir.split('|')[0]; rec_ir = ir.split('|')[1] if '|' in ir else ''
    rec_kh = kh.split('|')[1] if '|' in kh else ''
    a = nums(raw_ir, ['started', 'acked', 'lostjoin', 'failed', 'limited', 'nakd', 'nocover'])
    b = nums(rec_ir, ['started', 'acked', 'nakd', 'lostjoin', 'failed', 'nocover'])
    c = nums(rec_kh, ['served', 'nak'])
    k145 = ir.split('|')[2] if ir.count('|') >= 2 else ''
    p145 = nums(k145, ['fresh', 'freshsameip', 'old', 'retry', 'retryfail'])
    kmem = re.search(r'conns (\d+) \((\d+) KB\)', rec_kh)
    fails = [float(x) for x in open(d + 'fails.txt')] if os.path.exists(d + 'fails.txt') else []
    f1 = sum(1 for x in fails if 55 <= x < 135); f2 = sum(1 for x in fails if 135 <= x < 220); fq = sum(1 for x in fails if x < 55)
    lat = [tuple(map(float, l.split())) for l in open(d + 'lat.txt')] if os.path.exists(d + 'lat.txt') else []
    def p99(v):
        v = sorted(v); return v[min(len(v) - 1, int(len(v) * .99))] if v else -1
    lq = p99([v for s, v in lat if s < 55]); le = p99([v for s, v in lat if 55 <= s < 220]); la = p99([v for s, v in lat])
    rss = {'ir': 0, 'kh': 0}
    for l in open(d + 'proc.txt') if os.path.exists(d + 'proc.txt') else []:
        p = l.split()
        if len(p) >= 3 and p[1] in rss: rss[p[1]] = max(rss[p[1]], int(p[2].split('=')[1]) // 1024)
    rr = a['acked'] / a['started'] if a['started'] else -1
    rc = b['acked'] / b['started'] if b['started'] else -1
    tot = (a['acked'] + b['acked']) / (a['started'] + b['started']) if a['started'] + b['started'] else -1
    row = dict(tag=tag, raw_st=a['started'], raw_ack=a['acked'], raw_lost=a['lostjoin'], raw_fail=a['failed'], raw_lim=a['limited'],
               rec_st=b['started'], rec_ack=b['acked'], rec_nakd=b['nakd'], rec_lost=b['lostjoin'], kh_nak=c['nak'],
               kh_recMB=(int(kmem.group(2)) // 1024 if kmem else -1), retry=p145['retry'], rfail=p145['retryfail'], pf=p145['fresh'], pold=p145['old'],
               raw_ratio=round(rr, 3), rec_ratio=round(rc, 3), all_ratio=round(tot, 3), fails_q=fq, fails_ip1=f1, fails_ip2=f2,
               p99_q=lq, p99_ev=le, p99_all=la, n_lat=len(lat), rss_ir=rss['ir'], rss_kh=rss['kh'])
    rows.append(row)
    print(' '.join('%s=%s' % kv for kv in row.items()))
for arm in 'AB':
    rs = [r for r in rows if r['tag'].startswith(arm)]
    if not rs: continue
    keys = [k for k in rs[0] if k != 'tag']
    print('ARM %s n=%d ' % (arm, len(rs)) + ' '.join('%s=%.3g' % (k, st.mean(r[k] for r in rs)) for k in keys))

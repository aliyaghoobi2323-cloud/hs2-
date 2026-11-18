import re, sys, gzip, os, datetime
R = '/opt/hs2lab/ul268s/runs'
for n in sys.argv[1:]:
    d = os.path.join(R, n)
    L = gzip.open(d + '/logs/ir.full.log.gz', 'rt', errors='replace').read().splitlines()
    t0 = None; gcs = []
    day = datetime.datetime.fromtimestamp(os.path.getmtime(d + '/env.txt'), datetime.timezone.utc).date()
    for l in L:
        if t0 is None:
            m = re.match(r'(\d\d):(\d\d):(\d\d\.\d+)', l)
            if m:
                t0 = datetime.datetime(day.year, day.month, day.day, tzinfo=datetime.timezone.utc).timestamp() + int(m.group(1))*3600 + int(m.group(2))*60 + float(m.group(3))
        m = re.search(r'gc (\d+) @([\d.]+)s (\d+)%: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock.* (\d+)->(\d+)->(\d+) MB', l)
        if m:
            gcs.append((float(m.group(2)), float(m.group(4)) + float(m.group(6)), float(m.group(5)), int(m.group(8)), int(m.group(3))))
    # ping tails
    tails = []; allp = []
    for l in open(d + '/ping.txt'):
        m = re.match(r'\[([\d.]+)\].*time=([\d.]+)', l)
        if m:
            allp.append((float(m.group(1)), float(m.group(2))))
    if not gcs:
        print(n, 'no gctrace'); continue
    dur = gcs[-1][0] - gcs[0][0]
    stw = sorted(g[1] for g in gcs); mark = sorted(g[2] for g in gcs)
    print('%s gc n=%d per_s %.2f stw_max %.2f ms stw_sum %.1f ms mark_p50 %.0f ms mark_max %.0f heap_live_p50 %d MB gccpu_last %d%%' % (
        n, len(gcs), len(gcs)/max(dur,1), stw[-1], sum(stw), mark[len(mark)//2], mark[-1], sorted(g[3] for g in gcs)[len(gcs)//2], gcs[-1][4]))
    if allp and t0:
        # fraction of tail (>130 ms) samples sent during a GC mark phase (ping send time = recv - rtt)
        def ingc(ts):
            rel = ts - t0
            return any(g[0] - 0.05 <= rel <= g[0] + (g[2] + g[1]) / 1000 + 0.05 for g in gcs)
        tl = [(ts - rt/1000, rt) for ts, rt in allp if rt > 130]
        cov = sum((g[2] + g[1]) / 1000 + 0.1 for g in gcs) / max(1, allp[-1][0] - allp[0][0])
        print('   tail>130 n=%d in_gc(+-50ms) %d  (gc window covers %.1f%% of time)' % (len(tl), sum(1 for ts, _ in tl if ingc(ts)), 100*cov))

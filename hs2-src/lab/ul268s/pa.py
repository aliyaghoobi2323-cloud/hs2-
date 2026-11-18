import json, re, sys, os
R = '/opt/hs2lab/ul268s/runs'
def pc(v, q):
    v = sorted(v); return v[min(len(v)-1, int(q*len(v)))] if v else -1
for n in sys.argv[1:]:
    d = os.path.join(R, n)
    if not os.path.exists(d + '/DONE'): print(n, 'not done'); continue
    t = []; seqs = []
    if os.path.exists(d + '/ping.txt'):
        for l in open(d + '/ping.txt'):
            m = re.search(r'icmp_seq=(\d+) .*time=([\d.]+)', l)
            if m: seqs.append(int(m.group(1))); t.append(float(m.group(2)))
    sent = max(seqs) if seqs else 0
    lost = sent - len(seqs)
    j = json.load(open(d + '/result.json')); p = j['probe']
    over = lambda x: 100.0*sum(1 for v in t if v > x)/max(1,len(t))
    # burstiness: number of distinct 1 s windows holding samples > 150
    sl = sorted(set(int(s*0.05) for s, v in zip(seqs, t) if v > 150))
    print('%-4s n=%d lost=%d p50 %.1f p90 %.1f p99 %.1f p999 %.1f max %.0f >120 %.2f%% >150 %.2f%% secs>150 %d | crowdF %d c99 %.0f echo99 %.0f call99 %.0f callmax %.0f' % (
        n, len(t), lost, pc(t,.5), pc(t,.9), pc(t,.99), pc(t,.999), max(t) if t else -1, over(120), over(150), len(sl),
        p['crowd']['fail'], p['crowd']['p99_ms'], p['echo_p99_ms'], p['call']['p99_ms'], p['call']['max_ms']))

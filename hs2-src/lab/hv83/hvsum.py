#!/usr/bin/env python3
# hvsum.py RESDIR...: one block per hv83 run: light p50/p99 per class, per-user heavy throughput
# + fairness (Jain over fast users, LTE users as % of their own cap), total, LB verdicts
# (paths lowered and why, on each side), stripe gate/floor counters, links, CPU, RSS.
import json, sys, os, glob, re, statistics as st
def jain(x):
    x = [v for v in x if v is not None]
    return (sum(x) ** 2) / (len(x) * sum(v * v for v in x)) if x and sum(x) > 0 else float('nan')
def load(f):
    try: return json.load(open(f))
    except Exception: return None
for d in sys.argv[1:]:
    tag = os.path.basename(d.rstrip('/'))
    r = load(os.path.join(d, 'result.json')) or {}
    p = r.get('probe', {})
    cl = lambda k: p.get(k, {}) if isinstance(p.get(k), dict) else {}
    print('== %s  crowd p50/p95/p99=%.0f/%.0f/%.0f call p99=%.0f late%%=%.1f game p99=%.0f echo p99=%.0f conn p95=%.0f ping p99=%s fail=%s/%s' % (
        tag, cl('crowd').get('p50_ms', -1), cl('crowd').get('p95_ms', -1), cl('crowd').get('p99_ms', -1),
        cl('call').get('p99_ms', -1), cl('call').get('late_pct', -1), cl('game').get('p99_ms', -1),
        p.get('echo_p99_ms', -1), p.get('conn_p95_ms', -1), r.get('ping_p99'), cl('crowd').get('fail'), p.get('conn_fail')))
    users = []
    uf = os.path.join(d, 'hv', 'users.txt')
    if os.path.exists(uf):
        for ln in open(uf):
            a = ln.split()
            if len(a) < 6: continue
            uid, kind, stn, start, dur, rate = a
            j = load(os.path.join(d, 'hv', 'u%s.json' % uid))
            mb = None; ld = None
            if j and kind in 'fl': mb = j.get('mbps'); ld = j.get('new_ld_p90')
            elif j and kind == 'u':
                try: mb = j['end']['sum_received']['bits_per_second'] / 1e6
                except Exception: mb = None
            users.append((int(uid), kind, int(stn), float(start), int(dur), int(rate[:-4]), mb, ld))
    if users:
        tot = sum(u[6] or 0 for u in users)
        fast = [u[6] for u in users if u[1] == 'f']; fps = [u[6] / u[2] for u in users if u[1] == 'f' and u[6]]
        lte = [100 * u[6] / u[5] for u in users if u[1] == 'l' and u[6] is not None]
        up = [u[6] for u in users if u[1] == 'u']
        dead = [u[0] for u in users if not u[6] or u[6] < 5]
        q = lambda v: '%.0f/%.0f/%.0f' % (min(v), st.median(v), max(v)) if v else '-'
        print('   users=%d sum_mbps=%.0f | fast n=%d min/med/max=%s jain=%.2f per-stream jain=%.2f | lte %%cap min/med/max=%s | up n=%d %s | starved(<5M)=%s' % (
            len(users), tot, len(fast), q([x for x in fast if x is not None]), jain(fast), jain(fps), q(lte), len(up), q([x for x in up if x is not None]), dead))
        lds = [u[7] for u in users if u[7]]
        if lds: print('   heavy users own new-conn loaded p90 ms min/med/max=%s' % q(lds))
    for side, pat in (('ir', 'si_*.json'), ('kh', 'sk_*.json')):
        fs = sorted(glob.glob(os.path.join(d, 'hv', pat)), key=lambda f: int(re.findall(r'_(\d+)\.json', f)[0]))
        low, why, links, cpu, mb, gate, floor = 0, {}, [], [], [], {}, []
        for f in fs:
            s = load(f)
            if not s: continue
            links.append(s.get('links', 0)); cpu.append(s.get('cpu_pct', 0)); mb.append(s.get('lb_total', {}).get('down_mbit', 0) + s.get('lb_total', {}).get('up_mbit', 0))
            for pth in s.get('lb_report', {}).get('paths', []) or []:
                if pth.get('factor', 1) < 1:
                    low += 1; w = re.sub(r'[0-9.]+', '#', pth.get('reason', ''))
                    why[w] = why.get(w, 0) + 1
            for k in ('stripe_gate_cut', 'stripe_gate_closed', 'stripe_gate_grow', 'stripe_refused', 'aw_floor_subs', 'stripe_q_us', 'stripe_width_avg', 'smux_win_peak_mib'):
                if k in s: gate[k] = s[k]
        if not fs: continue
        print('   %s: samples=%d links min/max=%s/%s cpu%% med/max=%.0f/%.0f lb mbit med/max=%.0f/%.0f path-samples lowered=%d %s' % (
            side, len(fs), min(links), max(links), st.median(cpu), max(cpu), st.median(mb), max(mb), low, dict(sorted(why.items(), key=lambda x: -x[1])[:4])))
        print('      last counters %s' % gate)
    pf = os.path.join(d, 'hv', 'proc.txt')
    if os.path.exists(pf):
        rows = [list(map(float, l.split())) for l in open(pf) if len(l.split()) == 5]
        if len(rows) > 2:
            dt = rows[-1][0] - rows[0][0]
            print('   proc: cpu ir=%.0f%% kh=%.0f%% (avg cores*100) rss max ir=%.0f kh=%.0f MB' % (
                (rows[-1][1] - rows[0][1]) / dt, (rows[-1][2] - rows[0][2]) / dt, max(x[3] for x in rows) / 1024, max(x[4] for x in rows) / 1024))

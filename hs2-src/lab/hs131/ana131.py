#!/usr/bin/env python3
# ana131.py RESDIR...: hs131 per-side knock-out (arm from env.txt KHX/IRX), light p99, call episodes > 500 ms,
# per-side keepstall counters, gctrace (GODEBUG=gctrace=1) GC count / STW sum / GC cpu %, RSS.
import json, os, re, sys
def arm(env):
    kh = re.search(r'KHX=(.*?) IRX=', env).group(1); ir = re.search(r'IRX=(.*?) PEV=', env).group(1)
    a, b = 'KEEPSTALL=1' in kh, 'KEEPSTALL=1' in ir
    return {(0, 0): 'A_off', (1, 0): 'B_exit', (0, 1): 'C_iran', (1, 1): 'D_both'}[(a, b)]
for d in sys.argv[1:]:
    t = os.path.basename(d.rstrip('/'))
    try:
        r = json.load(open(f'{d}/result.json')); env = open(f'{d}/env.txt').read()
    except Exception:
        print(t, 'no result'); continue
    def txt(s):
        try: return open(f'{d}/{t}-{s}.log', errors='ignore').read()
        except Exception: return ''
    hv = r.get('heavy', {}) or {}; p = r.get('probe', {}) or {}
    if not hv.get('mbps'): print(t, arm(env), 'VOID noheavy'); continue
    v = []
    try:
        for l in open(f'{d}/samples.txt'):
            if l.startswith('k '):
                q = l.split(); v.append((float(q[1]), float(q[2])))
    except Exception: pass
    slow = [(a, x) for a, x in v if x > 500]; eps = []
    for a, x in slow:
        if eps and a - eps[-1][1] < 1.0: eps[-1][1] = a; eps[-1][3] = max(eps[-1][3], x)
        else: eps.append([a, a, 1, x])
    eplen = sum(e[1] - e[0] for e in eps)
    ks = {}
    for s in ('kh', 'ir'):
        L = re.findall(r'keepstall: .*', txt(s)); m = dict(re.findall(r'(\w+)=(\d+)', L[-1])) if L else {}
        ks[s] = '/'.join(m.get(k, '-') for k in ('gap_room', 'gap_fill', 'hole_sub', 'ack_stalls', 'targeted', 'resend_chunks'))
    gc = {}
    for s in ('kh', 'ir'):
        G = re.findall(r'gc \d+ @[\d.]+s (\d+)%: ([\d.]+)\+[\d.]+\+([\d.]+) ms clock', txt(s))
        gc[s] = f"{len(G)}/{sum(float(a)+float(c) for _, a, c in G):.0f}ms/{G[-1][0] if G else '-'}%"
    def fk(o, k):
        if isinstance(o, dict):
            if k in o: return o[k]
            for x in o.values():
                y = fk(x, k)
                if y is not None: return y
        return None
    print(f"{t} {arm(env)} mbps={hv.get('mbps')} call99={p.get('call',{}).get('p99_ms')} crowd99={p.get('crowd',{}).get('p99_ms')} "
          f"ping99={r.get('ping_p99')} echo99={p.get('echo_p99_ms')} game99={p.get('game',{}).get('p99_ms')} "
          f"slow500={len(slow)/max(len(v),1)*100:.2f}% eps={len(eps)} eplen={eplen:.1f} worst={max([e[3] for e in eps] or [0]):.0f} "
          f"rss_ir={fk(r,'rss_ir_mb')} rss_kh={fk(r,'rss_kh_mb')} cpu_kh={(hv.get('cpu') or {}).get('kh',{}).get('cores')} "
          f"gc_kh={gc['kh']} gc_ir={gc['ir']} ks_kh={ks['kh']} ks_ir={ks['ir']}")

#!/usr/bin/env python3
# ana121.py RESDIR...: per-run heavy + light p99 + keepstall closes (hs119)
import json, os, re, sys
for d in sys.argv[1:]:
    t = os.path.basename(d.rstrip('/'))
    try:
        r = json.load(open(f'{d}/result.json'))
    except Exception:
        print(t, 'no result'); continue
    env = open(f'{d}/env.txt').read()
    z = 0; err = ''
    try:
        h = json.load(open(f'{d}/{t}-heavy.json'))
        err = h.get('error') or ''
        for iv in h['intervals']:
            s = iv['sum']
            if 30 <= s['start'] < 150 and s['bits_per_second'] < 1e6: z += 1
    except Exception:
        err = 'noheavy'
    def txt(s):
        try: return open(f'{d}/{t}-{s}.log', errors='ignore').read()
        except Exception: return ''
    gap = sum(len(re.findall('missing bytes never arrived', txt(s))) for s in ('ir', 'kh'))
    cl = {s: re.findall(r'ack stall:.*', txt(s)) for s in ('kh', 'ir')}
    why = {s: [re.search(r'tcp=(\w+)', l).group(1) if 'tcp=' in l else '?' for l in cl[s]] for s in cl}
    cand = veto = hole = 0
    for s in ('kh', 'ir'):
        L = re.findall(r'keepstall: .*', txt(s))
        if L:
            m = dict(re.findall(r'(\w+)=(\d+)', L[-1])); cand += int(m.get('stall_cand', 0)); veto += int(m.get('stall_veto_tcp', 0)); hole += int(m.get('hole_sub', 0))
    p = r.get('probe', {}); hv = r.get('heavy', {})
    key = 'ON' if 'KEEPSTALL=1' in env else 'off'
    b = re.findall(r'BIN=\S*/(\S+) BIN_KH=\S*/(\S+)', env)
    print(f"{t} {key} {b[0][0]}/{b[0][1]} {hv.get('dir')}{hv.get('streams')} mbps={hv.get('mbps')} zero={z} gapRST={gap} "
          f"close_kh={len(cl['kh'])}{why['kh'] or ''} close_ir={len(cl['ir'])}{why['ir'] or ''} cand={cand} veto={veto} hole={hole} cpu_ir={(hv.get('cpu') or {}).get('ir',{}).get('cores')} cpu_kh={(hv.get('cpu') or {}).get('kh',{}).get('cores')} "
          f"ping99={r.get('ping_p99')} echo99={p.get('echo_p99_ms')} call99={p.get('call',{}).get('p99_ms')} "
          f"game99={p.get('game',{}).get('p99_ms')} crowd99={p.get('crowd',{}).get('p99_ms')} crowdfail={p.get('crowd',{}).get('fail')} err={err[:40]!r}")

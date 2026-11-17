#!/usr/bin/env python3
# ana113.py RESDIR...: heavy stall summary per run (hs113)
import json, os, re, sys
for d in sys.argv[1:]:
    t = os.path.basename(d.rstrip('/'))
    try:
        r = json.load(open(f'{d}/result.json'))
    except Exception:
        print(t, 'no result'); continue
    env = open(f'{d}/env.txt').read()
    z = z2 = 0; after = []; err = ''
    try:
        h = json.load(open(f'{d}/{t}-heavy.json'))
        err = h.get('error') or ''
        for iv in h['intervals']:
            s = iv['sum']; st = s['start']; bps = s['bits_per_second']
            if 30 <= st < 150 and bps < 1e6: z += 1
            if 90 <= st < 150: after.append(bps / 1e6)
        n = len(h['intervals'])
    except Exception as e:
        err = 'noheavy'; n = 0
    def cnt(pat, f):
        try: return len(re.findall(pat, open(f, errors='ignore').read()))
        except Exception: return -1
    gap = sum(cnt('missing bytes never arrived', f'{d}/{t}-{s}.log') for s in ('ir', 'kh'))
    st = sum(cnt('ack stall', f'{d}/{t}-{s}.log') for s in ('ir', 'kh'))
    ks = ''
    for s in ('kh', 'ir'):
        try:
            L = [l for l in open(f'{d}/{t}-{s}.log', errors='ignore') if 'keepstall: kept_marked' in l]
            if L: ks += f' {s}:' + re.sub(r'.*keepstall: ', '', L[-1]).strip()
        except Exception: pass
    p = r.get('probe', {}); c = p.get('call', {})
    hv = r.get('heavy', {})
    key = 'ON' if 'KEEPSTALL=1' in env else 'off'
    bins = re.findall(r'BIN=\S*/(\S+) BIN_KH=\S*/(\S+)', env)
    print(f"{t} {key} {bins} {hv.get('dir')} mbps={hv.get('mbps')} n_iv={n} zero_s(30-150)={z} after90={sum(after)/max(1,len(after)):.0f} gapRST={gap} ackstall={st} err={err[:50]!r} ping99={r.get('ping_p99')} echo99={p.get('echo_p99_ms')} call99={c.get('p99_ms')} late={c.get('late_pct',0):.1f}{ks}")

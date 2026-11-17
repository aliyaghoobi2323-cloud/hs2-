#!/usr/bin/env python3
# ana133.py RESDIR...: heavy mbps, zero-throughput seconds, mean after 95 s, light p99, crowd fails,
# RSS over time (rss.txt: max and at 30/60/90/120/150 s per side), keepstall + hs133 room counters.
import json, os, re, sys
for d in sys.argv[1:]:
    t = os.path.basename(d.rstrip('/'))
    try:
        r = json.load(open(f'{d}/result.json'))
    except Exception:
        print(t, 'no result'); continue
    hv = r.get('heavy', {}) or {}; p = r.get('probe', {}) or {}
    iv = []
    try:
        h = json.load(open(f'{d}/{t}-heavy.json'))
        for x in h.get('intervals', []):
            s = x['sum']; iv.append((s['start'], s['bits_per_second'] / 1e6))
    except Exception: pass
    zero = [a for a, b in iv if b < 1]
    after = [b for a, b in iv if a >= 95]
    rs = {'ir': {}, 'kh': {}}
    try:
        for l in open(f'{d}/rssb.txt' if os.path.exists(f'{d}/rssb.txt') else f'{d}/rss.txt'):
            q = l.split()
            if len(q) == 3: rs[q[1]][int(q[0])] = int(q[2])
    except Exception: pass
    def rr(s):
        m = rs[s]
        if not m: return '-'
        return f"max{max(m.values())} " + '/'.join(str(m.get(k, '-')) for k in (30, 60, 90, 120, 145))
    ks = {}
    for s in ('kh', 'ir'):
        try: L = re.findall(r'keepstall: .*', open(f'{d}/{t}-{s}.log', errors='ignore').read())
        except Exception: L = []
        m = dict(re.findall(r'(\w+)=(-?\d+)', L[-1])) if L else {}
        ks[s] = '/'.join(m.get(k, '-') for k in ('gap_room', 'gap_fill', 'hole_sub', 'room_old_peer', 'room_capped', 'room_bytes', 'resend_chunks'))
    zs = f"{len(zero)}s({zero[0]:.0f}-{zero[-1]:.0f})" if zero else '0s'
    print(f"{t} mbps={hv.get('mbps')} zero={zs} after95={sum(after)/len(after) if after else 0:.0f} "
          f"call99={p.get('call',{}).get('p99_ms')} crowd99={p.get('crowd',{}).get('p99_ms')} crowd_fail={p.get('crowd',{}).get('fail')} "
          f"rss_end ir={r.get('rss_ir_mb')} kh={r.get('rss_kh_mb')} | rss_ir {rr('ir')} | rss_kh {rr('kh')} | ks(gr/gf/hs/old/cap/bytes/rs) kh={ks['kh']} ir={ks['ir']}")

#!/usr/bin/env python3
# soak95sum.py DIR: bh95 quiet soak: silent readings / silent cuts / IP verdicts (must be 0 with no blackhole),
# every cut with its reason per side, the loss episodes that ran, crowd fails/churn, heavy users
import sys, os, re, glob, json, collections
d = sys.argv[1]
ev = open(os.path.join(d, 'ev.txt')).read().splitlines()
print('episodes:', sum(' drop ' in l for l in ev) - 1, 'starts,', sum(' undrop ' in l for l in ev), 'ends; first', ev[0][:15], 'last', ev[-1][:15])
for side, pat in (('kh', '*-kh.log'), ('ir', '*-ir.log')):
    fs = glob.glob(os.path.join(d, pat))
    if not fs: print(side, 'no log'); continue
    L = open(fs[0], errors='replace').read().splitlines()
    sil = [l for l in L if 'silent:' in l]
    scut = [l for l in L if re.search(r' cut \(.*silent', l)]
    ipv = [l for l in L if 'lossy on every Kharej IP' in l and 'Iran IP' in l and 'cut (' not in l]
    cuts = collections.Counter()
    for l in L:
        m = re.search(r'lb auto2: path (\S+) cut \((\S+)', l)
        if m: cuts[(m.group(1), m.group(2))] += 1
    print(f"{side}: lines={len(L)} first={L[0][:12] if L else '-'} last={L[-1][:12] if L else '-'} silent_readings={len(sil)} silent_cuts={len(scut)} ip_verdicts={len(ipv)} cuts={sum(cuts.values())}")
    for k, v in sorted(cuts.items()): print('   cut', k[0], k[1], v)
    for l in (sil + scut)[:10]: print('   !!', l[:200])
jr = json.load(open(os.path.join(d, 'result.json')))
c = jr['probe']['crowd']
print('crowd: fail', c['fail'], 'churn', c.get('churn'), 'n', c['n'], 'p99', c['p99_ms'], 'open', c['open'], '| call99', jr['probe'].get('call', {}).get('p99_ms'), 'game99', jr['probe'].get('game', {}).get('p99_ms'))
u = os.path.join(d, 'users.txt'); print('heavy users:', sum(1 for _ in open(u)) if os.path.exists(u) else 0)

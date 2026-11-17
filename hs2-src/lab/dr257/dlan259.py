# dl259 single-stream DL A/B: python3 dlan259.py RUN... (arm = first letter)
import re,json,gzip,statistics as st,sys,glob
R='/opt/hs2lab/dr257'; arm={}
for n in sys.argv[1:]:
    d=R+'/'+n
    try: L=open(d+'/tests.txt').read().splitlines()
    except: continue
    T=[]
    for l in L:
        m=re.search(r'dir='+re.escape(__import__('os').environ.get('DIR','-R'))+r' test_mbit=(\d+) \[([^\]]*)\]',l)
        if m: T.append((int(m.group(1)),[int(x) for x in m.group(2).split(',')]))
    r={}
    try: r=json.load(open(d+'/result.json'))
    except: pass
    p=r.get('probe',{}); c=p.get('call',{}); cr=r.get('crowd') or p.get('crowd') or {}
    pan=[open(d+'/%s.panics'%s).read().strip() if glob.glob(d+'/%s.panics'%s) else '?' for s in ('ir','kh')]
    g=[0,0,0]
    try:
        for l in gzip.open(d+'/logs/kh.full.log.gz','rt',errors='replace'):
            m=re.search(r'dl259 pathq 10s: gates=(\d+) path-cuts=(\d+) path-opens=(\d+)',l)
            if m: g=[g[i]+int(m.group(i+1)) for i in range(3)]
    except: pass
    tm=[t for t,_ in T]; s2=[x for _,iv in T for x in iv[2:]]
    print(f"== {n} n={len(T)} med={st.median(tm)} mean={st.mean(tm):.0f} min={min(tm)} max={max(tm)} sec2-11 p10={sorted(s2)[len(s2)//10]} | crowd p50/p99 {cr.get('p50_ms')}/{cr.get('p99_ms')} call99 {c.get('p99_ms')} echo99 {p.get('echo_p99_ms')} icmp99 {r.get('ping_p99')} rss ir/kh {r.get('rss_ir_mb')}/{r.get('rss_kh_mb')} panics {pan} dl259 gates/cuts/opens {g}")
    for t,iv in T: print('   ',t,iv)
    a=arm.setdefault(n[0],{'t':[],'k':[]}); a['t']+=tm
    a['k'].append((cr.get('p99_ms'),c.get('p99_ms'),p.get('echo_p99_ms'),r.get('ping_p99')))
for k,a in arm.items():
    print(f"ARM {k}: n={len(a['t'])} med={st.median(a['t'])} mean={st.mean(a['t']):.0f} | crowd/call/echo/icmp99 per run {a['k']}")

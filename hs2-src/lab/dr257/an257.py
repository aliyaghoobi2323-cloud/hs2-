import re,glob,json,statistics as st,sys
R='/opt/hs2lab/dr257'
arm={}
for n in sys.argv[1:]:
    d=R+'/'+n
    try: L=open(d+'/tests.txt').read().splitlines()
    except: continue
    ups=[]
    for l in L:
        m=re.search(r'dir=up test_mbit=(\d+) \[([^\]]*)\]',l)
        if m: ups.append((int(m.group(1)),[int(x) for x in m.group(2).split(',')]))
    try: r=json.load(open(d+'/result.json'))
    except: r={}
    p=r.get('probe',{}); c=p.get('call',{}); cr=r.get('crowd') or p.get('crowd') or {}
    pan=[open(d+'/%s.panics'%s).read().strip() if glob.glob(d+'/%s.panics'%s) else '?' for s in ('ir','kh')]
    import gzip
    try: ul=sum(1 for l in gzip.open(d+'/logs/ir.full.log.gz','rt',errors='replace') if 'ul257 stuck' in l)
    except: ul='?'
    s1=[x for _,s in ups for x in s[1:6]]
    stall=[t for t,s in ups if min(s[1:5])<50]
    print(f"== {n} ups={len(ups)} mean={st.mean([t for t,_ in ups]):.0f} med={st.median([t for t,_ in ups])} sec1-5 med={st.median(s1)} sec1-5<50: {sum(1 for x in s1 if x<50)}/{len(s1)} <100: {sum(1 for x in s1 if x<100)} stalltests={len(stall)} | crowd p50/p99 {cr.get('p50_ms')}/{cr.get('p99_ms')} call99 {c.get('p99_ms')} echo99 {p.get('echo_p99_ms')} icmp99 {r.get('ping_p99')} rss ir/kh {r.get('rss_ir_mb')}/{r.get('rss_kh_mb')} panics {pan} ul257lines={ul}")
    for t,s in ups:
        if min(s[1:5])<100: print('    ',t,s)
    a=arm.setdefault(n[0],{})
    for k,v in (('t',[t for t,_ in ups]),('s15',s1)): a.setdefault(k,[]).extend(v)
    for k,v in (('crowd99',cr.get('p99_ms')),('call99',c.get('p99_ms')),('echo99',p.get('echo_p99_ms')),('icmp99',r.get('ping_p99')),('rssir',r.get('rss_ir_mb'))):
        a.setdefault(k,[]).append(v)
for k,a in arm.items():
    print(f"ARM {k}: ups n={len(a['t'])} mean={st.mean(a['t']):.0f} med={st.median(a['t'])} sec1-5 med={st.median(a['s15'])} <50={sum(1 for x in a['s15'] if x<50)}/{len(a['s15'])} <100={sum(1 for x in a['s15'] if x<100)} | "+' '.join(f"{x}={a[x]}" for x in ('crowd99','call99','echo99','icmp99','rssir')))

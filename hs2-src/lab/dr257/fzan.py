# dr258 forced-stuck upload analyzer: python3 fzan.py RUN... (arm = first letter)
import re,json,gzip,statistics as st,sys,glob
R='/opt/hs2lab/dr257'; arm={}
for n in sys.argv[1:]:
    d=R+'/'+n
    try: L=open(d+'/tests.txt').read().splitlines()
    except: continue
    t0=int(open(d+'/t0').read()); day=t0-t0%86400
    tests=[]
    for l in L:
        m=re.search(r'start=([\d.]+) dir=(\w+) test_mbit=(\d+) \[([^\]]*)\]',l)
        if m: tests.append((float(m.group(1))+1.0,int(m.group(3)),[int(x) for x in m.group(4).split(',')]))
    trig=[]
    try:
        for l in gzip.open(d+'/logs/ir.full.log.gz','rt',errors='replace'):
            if 'ul257 stuck' in l:
                h,mi,s=l[:15].split(':'); x=day+int(h)*3600+int(mi)*60+float(s)
                if x<t0-3600: x+=86400
                trig.append((x,l.strip()))
    except Exception as e: pass
    tt=[];oth=[]
    for x,l in trig:
        k=[i for i,(s,_,_) in enumerate(tests) if s-0.5<=x<=s+13]
        (tt if k else oth).append((x,l,k))
    ttests={k[0] for _,_,k in tt}
    r={}
    try: r=json.load(open(d+'/result.json'))
    except: pass
    p=r.get('probe',{}); c=p.get('call',{}); cr=r.get('crowd') or p.get('crowd') or {}
    pan=[open(d+'/%s.panics'%s).read().strip() if glob.glob(d+'/%s.panics'%s) else '?' for s in ('ir','kh')]
    lo=[];stall=0;rec=[];per=[]
    for i,(s,tot,iv) in enumerate(tests):
        s15=iv[1:6]; z=sum(1 for v in s15 if v<50); lo.append(z)
        if z>=2: stall+=1
        f=next((j for j in range(1,len(iv)) if iv[j]<50),None)
        rc=0 if f is None else next((j for j in range(f,len(iv)) if iv[j]>=100),len(iv))
        rec.append(rc); per.append((i,tot,iv,i in ttests))
    print(f"== {n} tests={len(tests)} mean={st.mean(t for _,t,_ in tests):.0f} med={st.median(t for _,t,_ in tests)} s1-5<50 secs={sum(lo)}/{5*len(tests)} stalls(>=2s)={stall} recover-sec med={st.median(rec)} max={max(rec)} | ul257 test={len(tt)} (tests {len(ttests)}) other={len(oth)} | crowd p99 {cr.get('p99_ms')} call99 {c.get('p99_ms')} echo99 {p.get('echo_p99_ms')} icmp99 {r.get('ping_p99')} panics {pan}")
    for i,tot,iv,tr in per: print('   ',i,tot,iv,'UL257' if tr else '')
    for x,l,_ in oth: print('    OTHER',l)
    a=arm.setdefault(n[0],{'t':[],'lo':[],'stall':0,'n':0,'rec':[],'tt':0,'oth':0,'k':[]})
    a['t']+= [t for _,t,_ in tests]; a['lo']+=lo; a['stall']+=stall; a['n']+=len(tests); a['rec']+=rec; a['tt']+=len(tt); a['oth']+=len(oth)
    a['k'].append((cr.get('p99_ms'),c.get('p99_ms'),p.get('echo_p99_ms'),r.get('ping_p99'),r.get('rss_ir_mb')))
for k,a in arm.items():
    print(f"ARM {k}: n={a['n']} mean={st.mean(a['t']):.0f} med={st.median(a['t'])} s1-5<50 secs={sum(a['lo'])}/{5*a['n']} stalls={a['stall']} recover med={st.median(a['rec'])} mean={st.mean(a['rec']):.1f} ul257 test={a['tt']} other={a['oth']} | crowd/call/echo/icmp99/rssir per run {a['k']}")

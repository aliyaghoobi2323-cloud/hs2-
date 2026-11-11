import json,os,re,sys,random,statistics as st,math
D=sys.argv[1]; A,B=sys.argv[2],sys.argv[3]
g={}
for n in sorted(os.listdir(D)):
    m=re.match(r"(c1|u8)-(\w+)-(\d+)$",n)
    if not m or m.group(2) not in (A,B): continue
    try: r=json.load(open(D+n+"/result.json"))
    except Exception: continue
    h=r.get("heavy") or {}; c=r["probe"].get("crowd") or {}
    g.setdefault(m.group(1),{}).setdefault(m.group(2),[]).append(dict(ping99=r.get("ping_p99"),c99=c.get("p99_ms"),mbps=h.get("mbps"),echo99=r["probe"].get("echo_p99_ms"),call99=(r["probe"].get("call") or {}).get("p99_ms"),game99=(r["probe"].get("game") or {}).get("p99_ms")))
def mw(x,y):
    allv=sorted([(v,0) for v in x]+[(v,1) for v in y]); n=len(allv); ranks=[0]*n; i=0
    while i<n:
        j=i
        while j+1<n and allv[j+1][0]==allv[i][0]: j+=1
        for k in range(i,j+1): ranks[k]=(i+j)/2+1
        i=j+1
    R1=sum(r for r,(v,s) in zip(ranks,allv) if s==0); n1,n2=len(x),len(y)
    U=R1-n1*(n1+1)/2
    # exact-ish permutation p (two-sided, on U)
    pool=x+y; obs=abs(U-n1*n2/2); hit=0; N=20000; rnd=random.Random(1)
    for _ in range(N):
        rnd.shuffle(pool); xs=pool[:n1]
        s=sorted([(v,0) for v in xs]+[(v,1) for v in pool[n1:]])
        # rank sum with ties
        rk={}; i=0
        while i<len(s):
            j=i
            while j+1<len(s) and s[j+1][0]==s[i][0]: j+=1
            for k in range(i,j+1): rk.setdefault(k,(i+j)/2+1)
            i=j+1
        u=sum(rk[k] for k,(v,t) in enumerate(s) if t==0)-n1*(n1+1)/2
        if abs(u-n1*n2/2)>=obs-1e-9: hit+=1
    return U,hit/N
for var in sorted(g):
    print("== %s  %s n=%d  %s n=%d"%(var,A,len(g[var].get(A,[])),B,len(g[var].get(B,[]))))
    for k in ("ping99","echo99","call99","game99","c99","mbps"):
        x=[d[k] for d in g[var].get(A,[]) if d[k] is not None]; y=[d[k] for d in g[var].get(B,[]) if d[k] is not None]
        if len(x)<2 or len(y)<2: continue
        U,p=mw(x,y)
        print("  %-7s %s med %.1f mean %.1f sd %.1f | %s med %.1f mean %.1f sd %.1f | diff(mean) %+.1f  MW U=%.1f p=%.3f"%(k,A,st.median(x),st.mean(x),st.stdev(x),B,st.median(y),st.mean(y),st.stdev(y),st.mean(x)-st.mean(y),U,p))

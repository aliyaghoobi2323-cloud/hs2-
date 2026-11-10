import json,sys,itertools,statistics as st,re
R=sys.argv[1]; import os; RT=int(os.environ.get("REC","60"))
def mw(a,b):
    allv=a+b; n=len(a)
    if not a or not b: return float('nan')
    U=lambda x,y: sum((xi>yi)+0.5*(xi==yi) for xi in x for yi in y)
    u0=U(a,b); c=tot=0
    for idx in itertools.combinations(range(len(allv)),n):
        s=set(idx); x=[allv[i] for i in idx]; y=[allv[i] for i in range(len(allv)) if i not in s]
        tot+=1; c+= abs(U(x,y)-n*len(b)/2)>=abs(u0-n*len(b)/2)-1e-9
    return c/tot
def hmean(tag):
    try:
        d=json.load(open(f"{R}/logs/{tag}-heavy.json")); b=[i["sum"]["bits_per_second"]/1e6 for i in d.get("intervals",[]) if not i["sum"].get("omitted")]
        return sum(b)/len(b) if b else float('nan')
    except Exception: return float('nan')
def bal(tag):
    # seconds after the recovery event (t=REC, default 60) until cut path b (192.168.51.1) holds >=75% of the mean links of a and c, for 5 s in a row
    rows=[json.loads(l) for l in open(f"{R}/logs/{tag}-lbsamp.jsonl")]
    rec=None; ok=0; r90=None; ok9=0; minb=None; series=[]
    for r in rows:
        t=r["t"]; L={p["local"]:p["links"] for p in r.get("ir",[])}
        b=L.get("192.168.51.1",0); o=(L.get("192.168.50.1",0)+L.get("192.168.52.1",0))/2
        if t>=RT: series.append((round(t),b,o))
        if t>=RT and rec is None:
            ok = ok+1 if o>0 and b>=0.75*o else 0
            if ok>=5: rec=t-RT-4
        if t>=RT and r90 is None:
            ok9 = ok9+1 if o>0 and b>=0.9*o else 0
            if ok9>=5: r90=t-RT-4
    end=series[-1] if series else None
    gap=[max(0,1-b/o) for t,b,o in series if o>0]
    global GAP,R90; R90=r90 if r90 is not None else 999; GAP=100*sum(gap)/len(gap) if gap else float('nan')
    return (rec if rec is not None else 999), end, series
def tchg(tag):
    return sum(1 for l in open(f"{R}/logs/{tag}-kh.log") if "exit pool target" in l)
def fastr(tag):
    try: return open(f"{R}/logs/{tag}-ir.log").read().count("fast recovery")+open(f"{R}/logs/{tag}-kh.log").read().count("fast recovery")
    except Exception: return -1
def spare(tag):
    s=open(f"{R}/logs/{tag}-kh.log").read()
    return s.count("spare dial asked"), s.count("spare dial started"), s.count("exit link up")
runs=[json.loads(l) for l in open(f"{R}/runs.jsonl")]
M={}
for r in runs:
    if not r["res"]: print(r["tag"],"FAIL"); continue
    p=r["res"]["probe"]; tag=r["tag"]
    rec,end,ser=bal(tag)
    bs=[b for t,b,o in ser]
    m=dict(t75=rec, t90=R90, gap=GAP, bmin=min(bs), bend=bs[-1], gain=bs[-1]-min(bs), tchg=tchg(tag), c95=p["crowd"]["p95_ms"], c99=p["crowd"]["p99_ms"], call99=p["call"]["p99_ms"], ping99=r["res"].get("ping_p99",float("nan")), game99=p["game"]["p99_ms"], heavy=hmean(tag), fastr=fastr(tag))
    M.setdefault(r["arm"],[]).append(m)
    print(tag, {k:(round(v,1) if isinstance(v,float) else v) for k,v in m.items()}, "end b/oth", end, "spare asked/started/up", spare(tag))
    print("   b/others every 10s:", [(t,b,round(o)) for t,b,o in ser[::10]])
arms=list(M)
print()
for k in ["t75","t90","gap","bmin","bend","gain","tchg","c95","c99","call99","ping99","game99","heavy","fastr"]:
    line=f"{k:7s}"
    for a in arms: line+=f"  {a}: med {st.median([x[k] for x in M[a]]):.1f}"
    F=sys.argv[2] if len(sys.argv)>2 else "fix"
    if "base" in M and F in M: line+=f"  p(base vs {F})={mw([x[k] for x in M['base']],[x[k] for x in M[F]]):.2f}"
    print(line)

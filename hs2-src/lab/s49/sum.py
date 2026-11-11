import json,os,re,sys,statistics as st
D=sys.argv[1]; g={}
for n in sorted(os.listdir(D)):
    m=re.match(r"(c1|c0|u8|u1|s)-(\w+)-[ab]\d+$",n)
    if not m: continue
    try: r=json.load(open(f"{D}/{n}/result.json"))
    except Exception: continue
    h=r.get("heavy") or {}; p=r.get("probe") or {}; c=p.get("crowd") or {}
    fz=None; post=None
    try:
        iv=json.load(open(f"{D}/{n}/{n}-heavy.json"))["intervals"]; s=[i["sum"]["bits_per_second"]/1e6 for i in iv]
        if m.group(1)=="u8" and len(s)>16: post=st.mean(s[14:]); fz=int(max(s[-5:])<5)
    except Exception: pass
    g.setdefault((m.group(1),m.group(2)),[]).append(dict(mbps=h.get("mbps"),post=post,fz=fz,ping99=r.get("ping_p99"),c99=c.get("p99_ms"),cfail=c.get("fail"),echo99=p.get("echo_p99_ms"),call99=(p.get("call") or {}).get("p99_ms"),game99=(p.get("game") or {}).get("p99_ms")))
def f(v):
    v=[x for x in v if x is not None]
    return f"{st.mean(v):.0f}[{st.median(v):.0f}]" if v else "-"
print("var arm n frozen | mbps post-kill-mbps ping99 crowd99 crowdfail echo99 call99 game99 (mean[median])")
for k in sorted(g):
    L=g[k]; fz=[d["fz"] for d in L if d["fz"] is not None]
    print(k[0],k[1],len(L),f"{sum(fz)}/{len(fz)}" if fz else "-","|"," ".join(f([d[x] for d in L]) for x in ("mbps","post","ping99","c99","cfail","echo99","call99","game99")))

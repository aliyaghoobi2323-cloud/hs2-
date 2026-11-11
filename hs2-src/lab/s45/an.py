import json,os,re,statistics as st,sys
D=sys.argv[1] if len(sys.argv)>1 else "/opt/hs2lab/s45/res/"
g={}
for n in sorted(os.listdir(D)):
    m=re.match(r"(\w+)-(tls|reality|both|r70|rpy|an|anb)-(\d+)$",n)
    if not m: continue
    try: r=json.load(open(D+n+"/result.json"))
    except Exception: continue
    h=r.get("heavy") or {}; c=r["probe"].get("crowd") or {}
    if not h.get("mbps") and not r.get("kill"): continue
    cpu=h.get("cpu",{}) if h.get("mbps") else {}; gb=(h.get("mbps") or 0)/1000
    su=None; fr=None; t100=None
    for l in open(D+n+"/ir-status.jsonl"):
        try: x=json.loads(l)
        except Exception: continue
        if fr is None and (x.get("links") or 0)>0: fr=x["t"]
        if su is None and (x.get("links") or 0)>=150: su=x["t"]
        if t100 is None and (x.get("links") or 0)>=100: t100=x["t"]
    k=r.get("kill") or {}
    d=dict(mbps=h.get("mbps") or None,c95=c.get("p95_ms"),c99=c.get("p99_ms"),cfail=c.get("fail"),ping99=r.get("ping_p99"),
           cpu_ir=cpu.get("ir",{}).get("cores"),cpu_kh=cpu.get("kh",{}).get("cores"),
           irpg=cpu.get("ir",{}).get("cores",0)/gb if gb else None,khpg=cpu.get("kh",{}).get("cores",0)/gb if gb else None,
           first=fr,setup100=(t100-fr) if (t100 and fr) else None,setup150=su,k_t50=k.get("rebuild_t50_s"),k_t90=k.get("rebuild_t90_s"),k_t100=k.get("rebuild_t100_s"),conn50=r["probe"].get("conn_p50_ms"),conn95=r["probe"].get("conn_p95_ms"))
    g.setdefault(m.group(1),{}).setdefault(m.group(2),[]).append(d)
def ms(v):
    v=[x for x in v if x is not None and x!=-1]
    return "%.1f [%.1f-%.1f]"%(st.median(v),min(v),max(v)) if v else "-"
for var in sorted(g):
    keys=["mbps","c95","c99","ping99","cpu_ir","cpu_kh","irpg","khpg","first","setup100","setup150","conn50","conn95"]+(["k_t50","k_t90","k_t100"] if var=="u8" else [])
    print("== %s"%var+"".join("  %s n=%d"%(a,len(g[var].get(a,[]))) for a in ("tls","reality","both","r70","an","anb")))
    for k in keys:
        print("  %-9s"%k+"".join("%-24s"%ms([x[k] for x in g[var].get(a,[])]) for a in ("tls","reality","both","r70","an","anb")))

#!/usr/bin/env python3
# an47.py RESDIR — per-run table + per-arm medians for the rel47 matrix
import json,os,re,glob,sys,statistics as st
D=sys.argv[1].rstrip("/")+"/"
rows={}
for n in sorted(os.listdir(D)):
    try: r=json.load(open(D+n+"/result.json"))
    except Exception: continue
    p=r.get("probe",{}); c=p.get("crowd",{}) or {}; call=p.get("call",{}) or {}; g=p.get("game",{}) or {}; h=r.get("heavy") or {}
    kh="".join(open(f,errors="replace").read() for f in glob.glob(D+n+"/*-kh.log"))
    ir="".join(open(f,errors="replace").read() for f in glob.glob(D+n+"/*-ir.log"))
    fails=sum(int(p.get(k,0) or 0) for k in ("conn_fail","echo_lost"))+int(c.get("fail",0) or 0)+int(call.get("lost",0) or 0)+int(g.get("lost",0) or 0)
    cool=[re.sub(r".*lb: path \S+>(\S+) cooled for (\S+) after (\d+).*",r"\1 \2/\3",l) for l in kh.splitlines() if "cooled for" in l]
    back=[re.sub(r".*lb: path \S+>(\S+) is back.*",r"\1",l) for l in kh.splitlines() if "is back" in l]
    cut=len(re.findall(r"lb: path \S+ (?:cut|lowered|rescue)",kh))
    proof=kh.count("reading server proof")
    fb=len(re.findall(r"dialing tls for",kh))
    last=None
    for l in open(D+n+"/ir-status.jsonl"):
        try: last=json.loads(l)
        except Exception: pass
    fr=""
    if last and last.get("fronts"):
        fr=" ".join("%s:%s r%s t%s c%s x%s"%(f["ip"].split(".")[2],f["kind"][:4],f.get("counters",{}).get("reality",0),f.get("counters",{}).get("tls",0),f.get("counters",{}).get("cover",0),f.get("counters",{}).get("rejected",0)) for f in last["fronts"])
    rs=(last or {}).get("reality") or {}
    rej={k:v for k,v in (rs.get("rejected") or rs.get("reject") or {}).items()} if isinstance(rs,dict) else {}
    d=dict(n=n,c99=c.get("p99_ms",0),call=call.get("p99_ms",0),game=g.get("p99_ms",0),ping=r.get("ping_p99") or 0,mb=h.get("mbps",0),fails=fails,paths=r.get("lb_paths_end"),cool=cool,back=back,proof=proof,fb=fb,fr=fr,rej=rej,late=rs.get("target_hello_late") if isinstance(rs,dict) else None,churn=r.get("link_churn"))
    rows.setdefault(re.sub(r"-\d+$","",n),[]).append(d)
for a,v in rows.items():
    for d in v: print(d["n"],"c99=%s call=%s game=%s ping=%s mb=%s fails=%s paths=%s churn=%s cool=%s back=%s proof=%s fb=%s | %s | rej=%s late=%s"%(d["c99"],d["call"],d["game"],d["ping"],d["mb"],d["fails"],d["paths"],d["churn"],d["cool"],d["back"],d["proof"],d["fb"],d["fr"],d["rej"],d["late"]))
print()
for a,v in rows.items():
    m=lambda k:"%.0f[%.0f-%.0f]"%(st.median([x[k] for x in v]),min(x[k] for x in v),max(x[k] for x in v))
    print("%-8s n=%d crowd99 %s call99 %s game99 %s ping99 %s Mbps %s fails %s"%(a,len(v),m("c99"),m("call"),m("game"),m("ping"),m("mb"),sum(x["fails"] for x in v)))

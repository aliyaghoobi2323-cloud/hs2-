import json,os,re,sys
D=sys.argv[1]
for n in sorted(os.listdir(D)):
    if not re.match(r"c1-(an|anb|r70|rpy|reality|tls)-\d+$",n): continue
    p=D+n+"/"
    kh=open(p+n+"-kh.log",errors="replace").read() if os.path.exists(p+n+"-kh.log") else ""
    ir=open(p+n+"-ir.log",errors="replace").read() if os.path.exists(p+n+"-ir.log") else ""
    proof=kh.count("reading server proof"); tans=kh.count("answered by the reality target"); grace=ir.count("switch grace")
    fr=s100=s150=None; last=None
    for l in open(p+"ir-status.jsonl"):
        try: x=json.loads(l)
        except Exception: continue
        L=x.get("links") or 0
        if fr is None and L>0: fr=x["t"]
        if s100 is None and L>=100: s100=x["t"]
        if s150 is None and L>=150: s150=x["t"]
        last=x
    cnt={}
    for f in (last or {}).get("fronts") or []:
        for k,v in (f.get("counters") or {}).items(): cnt[k]=cnt.get(k,0)+v
    try: r=json.load(open(p+"result.json")); ping=r.get("ping_p99"); c99=r["probe"]["crowd"]["p99_ms"]; mb=(r.get("heavy") or {}).get("mbps")
    except Exception: ping=c99=mb=None
    print("%-14s first %s t100 %s t150 %s | proof_timeouts %d target_answered %d grace_logs %d | counters %s | ping99 %s c99 %s mbps %s"%(n,fr,s100,s150,proof,tans,grace,cnt,ping,c99,mb))

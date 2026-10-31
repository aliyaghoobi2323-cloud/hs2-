#!/usr/bin/env python3
# lowb.py RAW...: per run, the budget lines of the status at the end (exit, edge): links with a small B
import json,re,sys,os,glob
for raw in sys.argv[1:]:
    for l in open(raw+"/res.jsonl"):
        r=json.loads(l)
        if not r.get("ok") or r["bin"] in ("prod",): continue
        d=raw+"/raw/"+r["id"]
        out=[]
        for side in ("exit","edge"):
            try: t=open(d+"/status-%s.txt"%side,errors="replace").read()
            except: continue
            bs=re.findall(r"budget link (\d+): (\w+) ([a-z\- ()]*?), B (\d+) KiB \(in flight at most (\d+) KiB\), queue ([\d.]+) ms \(srtt-min ([\d.]+), way back ([\d.]+), target [\d.]+\), delivery ([\d.]+) Mbit/s, cuts (\d+), grows (\d+)",t)
            n=len(bs); low=[(b[0],b[2][:3],int(b[3]),float(b[6]),float(b[8])) for b in bs if b[1]=="run" and int(b[3])<=16]
            out.append("%s %d lines, low %s"%(side,n,low))
        w=r["classes"]["web"]["main"] if "web" in r["classes"] else {}
        print(r["id"],"N",r["N"],"loss",r["LOSS"],"web fail",w.get("failed"),"p95",w.get("load_p95_ms"),"|"," | ".join(out))

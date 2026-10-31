#!/usr/bin/env python3
# qseries.py RUN SIDE: per socket, every 5 s: cc cwnd ssthresh rtt-min acked Mbit/s retr notsent sendq
import re,sys,os
run,side=sys.argv[1],sys.argv[2]
t0=float(open(run+"/client-t0.txt").read().split()[0])
cur=None;last={}
hdr=None
rows={}
for line in open(run+"/sstrace-%s.txt"%side,errors="replace"):
    if line.startswith("T "):
        cur=float(line.split()[1])-t0;continue
    if line[0] in "\t ":
        if not hdr: continue
        d=line.strip();cc=d.split()[0]
        g=lambda p,f=float:(lambda m:f(m.group(1)) if m else 0)(re.search(p,d))
        k=hdr[3]
        if cc=="bbr": k=k+"B"
        rows.setdefault(k,[]).append((cur,cc,g(r"cwnd:(\d+)",int),g(r"ssthresh:(\d+)",int),g(r"rtt:([\d.]+)/"),g(r"minrtt:([\d.]+)"),g(r"bytes_acked:(\d+)",int),g(r"retrans:\d+/(\d+)",int),g(r"notsent:(\d+)",int),hdr[1],g(r"bytes_received:(\d+)",int)))
        hdr=None
    else:
        p=line.split()
        if len(p)>=4: hdr=(p[0],int(p[1]),p[2],p[3])
for k,v in rows.items():
    print("==",k,v[0][1])
    prev=None
    for r in v:
        if prev is None or r[0]-prev[0]>=4.9:
            if prev is not None:
                dt=r[0]-prev[0]
                print("t=%5.1f %-8s cwnd %4d ssth %4d q %5.1f acked %5.1f rx %5.1f Mbit/s retr %4d notsent %6d sendq %7d"%(r[0],r[1],r[2],r[3],r[4]-r[5],(r[6]-prev[6])*8/dt/1e6,(r[10]-prev[10])*8/dt/1e6,r[7]-prev[7],r[8],r[9]))
            prev=r

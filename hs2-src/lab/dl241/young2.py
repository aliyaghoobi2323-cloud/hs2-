import sys,gzip,re
# per stripe conn: first "striped over 2 subs" time; max oooB in Iran tick lines within 3 s after; count conns with >=4MB/>=8MB
def ts(s):
    h,m,x=s.split(":"); return int(h)*3600+int(m)*60+float(x)
for f in sys.argv[1:]:
    start={}; mx={}
    for l in gzip.open(f,"rt",errors="replace"):
        p=l.split(" ",3)
        if len(p)<3: continue
        mm=re.match(r"mconn ([0-9a-f]+): striped over 2 subs",l[16:])
        if mm: start.setdefault(mm.group(1),ts(p[0])); continue
        mm=re.match(r"stripe ([0-9a-f]+): tick iran=true .*oooB=(\d+)",l[16:])
        if mm and mm.group(1) in start:
            t=ts(p[0])-start[mm.group(1)]
            if 0<=t<=3.5: mx[mm.group(1)]=max(mx.get(mm.group(1),0),int(mm.group(2)))
    n=len(start); a=sum(1 for v in mx.values() if v>=4<<20); b=sum(1 for v in mx.values() if v>=8<<20)
    print(sorted((k,round(v/1e6,1)) for k,v in mx.items() if v>=4<<20)); print(f.split("/")[-3], "stripes",n,"young-HOL oooB>=4MB",a,">=8MB",b)

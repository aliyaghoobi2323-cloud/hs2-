#!/bin/bash
# anc84.sh: HS2_LB_PATH_SILENT A/B (res/O* off, res/C* on): blackhole b 40-80 s, c 160-200 s
cd /opt/hs2lab/lb84/res
printf "%-3s %6s %6s %6s %8s %8s %7s %7s %6s %s\n" run fB fC fAll c99all c99ev call99 game99 heavy silent-marks
for t in $(ls -d O* C* 2>/dev/null | sort -k1.2n -k1.1); do
  [ -e $t/result.json ] || continue
  sm=$(grep -h "lb path-silent" $t/ir.log $t/kh.log 2>/dev/null | sed 's/.*path(s) \(\[[^]]*\]\).*/\1/' | tr '\n' ' ')
  python3 - $t "$sm" <<'PY'
import json,sys
t,sm=sys.argv[1:]
F=[];C=[]
for ln in open(t+"/samples.txt"):
    p=ln.split()
    if len(p)<3: continue
    if p[0]=="F": F.append(float(p[1]))
    elif p[0]=="c": C.append((float(p[1]),float(p[2])))
fb=sum(1 for s in F if 40<=s<100); fc=sum(1 for s in F if 160<=s<220)
def p99(v):
    v=sorted(v); return round(v[int(len(v)*0.99)],1) if v else None
ev=[m for s,m in C if 40<=s<100 or 160<=s<220]
r=json.loads(open(t+"/result.json").read()); p=r.get("probe") or {}; k=p.get("call") or {}; g=p.get("game") or {}
try: hv=round(json.load(open(t+"/hv-d.json"))["end"]["sum_received"]["bits_per_second"]/1e6)
except Exception: hv=None
print("%-3s %6d %6d %6d %8s %8s %7s %7s %6s %s"%(t,fb,fc,len(F),p99([m for s,m in C]),p99(ev),k.get("p99_ms"),g.get("p99_ms"),hv,sm))
PY
done

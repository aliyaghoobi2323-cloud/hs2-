#!/bin/bash
# an84.sh: one line per lb84 run (res/A* = keys off, res/B* = ARMB)
cd /opt/hs2lab/lb84/res
printf "%-4s %6s %6s %6s %5s %7s %7s %7s %6s %6s %7s %s\n" run flipsD loweredD trickl cutA crowd99 game99 call99 cfail cfailE heavyM call_end
for t in $(ls -d [AB]* | sort -k1.2n -k1.1); do
  [ -e $t/result.json ] || continue
  fl=$(grep -h "lb auto2: weight factor" $t/kh.log $t/ir.log 2>/dev/null | grep -o "192.168.53.1 [0-9.]*→[0-9.]*" | wc -l)
  lw=$(grep -h "lb auto2: weight factor" $t/kh.log $t/ir.log 2>/dev/null | grep -o "192.168.53.1 1.00→[0-9.]*" | wc -l)
  tr=$(grep -h "closed .* on retiring links" $t/ir.log $t/kh.log 2>/dev/null | grep -o "[0-9]* trickling" | awk '{s+=$1} END{print s+0}')
  cut=$(grep -hc "cut-drain: .*link(s) on cut paths retired" $t/ir.log 2>/dev/null)
  ce=$(awk '$1=="F" && $2>=40 && $2<120' $t/samples.txt 2>/dev/null | wc -l)
  python3 - $t $fl $lw $tr $cut $ce <<'PY'
import json,sys
t,fl,lw,tr,cut,ce=sys.argv[1:]
try: r=json.loads(open(t+"/result.json").read())
except Exception: r={}
p=r.get("probe") or {}; c=p.get("crowd",{}) or {}; k=p.get("call") or {}; g=p.get("game") or {}
try: hv=round(json.load(open(t+"/hv-d.json"))["end"]["sum_received"]["bits_per_second"]/1e6)
except Exception: hv=None
print("%-4s %6s %6s %6s %5s %7s %7s %7s %6s %6s %7s %s"%(t,fl,lw,tr,cut,c.get("p99_ms"),g.get("p99_ms"),k.get("p99_ms"),c.get("fail"),ce,hv,(k.get("error") or "ok")[:30]))
PY
done

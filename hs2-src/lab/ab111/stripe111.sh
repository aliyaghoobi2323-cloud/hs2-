#!/bin/bash
# stripe111.sh MODE: per run heavy Mbit/s + stripe growth (edge log): ids striped, final subs per id (sorted), seconds from first "over 2" to each id's last grow
R=/opt/hs2lab/ab111/res; M=${1:-d8}
for d in $R/$M*; do t=$(basename $d); [ -f $d/result.json ] || continue
  hv=$(python3 -c "import json;print(round(json.load(open('$d/result.json')).get('heavy',{}).get('mbps',0)))" 2>/dev/null)
  zcat $d/$t-ir.log.gz 2>/dev/null | python3 -c "
import sys,re
F={};t0=None
for l in sys.stdin:
  m=re.match(r'(\d+):(\d+):([\d.]+) mconn (\w+): striped over (\d+) subs',l)
  if not m: continue
  s=int(m.group(1))*3600+int(m.group(2))*60+float(m.group(3)); t0=t0 or s
  F[m.group(4)]=(int(m.group(5)),round(s-t0,1))
print('$t',$hv or 0,'ids',len(F),'subs',sorted(v[0] for v in F.values()),'sum',sum(v[0] for v in F.values()),'lastgrow',max([v[1] for v in F.values()] or [0]))"
done | sort -k2 -n

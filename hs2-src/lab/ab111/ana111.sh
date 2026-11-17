#!/bin/bash
# ana111.sh [MODE]: per run heavy Mbit/s + log counters (both sides) to find why low runs are low.
# cols: tag mbps | ir: stripe lines, max subs, retiring, pattern, cut-ish, linkdown, rescue triggers/started | kh: same
set -u
R=/opt/hs2lab/ab111/res; M=${1:-d8}
for d in $R/$M*; do t=$(basename $d); [ -f $d/result.json ] || continue
  hv=$(python3 -c "import json;print(round(json.load(open('$d/result.json')).get('heavy',{}).get('mbps',0)))" 2>/dev/null)
  o="$t $hv"
  for s in ir kh; do f=$d/$t-$s.log.gz; [ -f $f ] || { o="$o | -"; continue; }
    zcat $f | awk -v s=$s '
      /striped over/ {st++; n=$(NF-1); if (n>mx) mx=n}
      /retiring/ {re++} /pattern / {pa++}
      /[Cc]ut|silent/ && !/rescue: rings/ {cu++}
      / down: / {dn++}
      /mconn rescue: rings/ {match($0,/triggers [0-9]+/); tr=substr($0,RSTART+9,RLENGTH-9); match($0,/started [0-9]+/); sa=substr($0,RSTART+8,RLENGTH-8)}
      /floor/ {fl++}
      END {printf " | %s st%d mx%d re%d pa%d cut%d dn%d fl%d resc%s/%s", s, st, mx, re, pa, cu, dn, fl, tr, sa}' > /tmp/a111.o; o="$o$(cat /tmp/a111.o)"
  done; echo "$o"
done | sort -k2 -n

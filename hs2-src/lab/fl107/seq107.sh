#!/bin/bash
# seq107.sh WORKER NW JOBFILE: job lines "TAG ARM SC [HSBIN [HSBIN_KH]]", (n % NW == WORKER), one at a time in a lab slot.
# Never edit this file or d107.sh while a queue runs (bash reads scripts lazily) — write new copies.
set -u
D=/opt/hs2lab/fl107; w=$1; nw=$2; n=0
while read -r t a sc hb hk; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % nw)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  HSBIN=${hb:-hs2-c} HSBIN_KH=${hk:-${hb:-hs2-c}} /opt/hs2lab/labslot.sh bash $D/d107.sh $t $a $sc
  echo "$(date +%T) w$w $t" >> $D/res/seq.log
done < $3
echo ALLDONE > $D/res/ALLDONE.w$w

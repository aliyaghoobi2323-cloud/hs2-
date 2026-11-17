#!/bin/bash
# seq101.sh WORKER NW JOBFILE: lines with (n % NW == WORKER), one at a time in any free single lab slot
set -u
D=/opt/hs2lab/bh100; w=$1; nw=$2; n=0
while read -r t a u sc s; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % nw)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  SEED=${s:-95} /opt/hs2lab/labslot.sh $D/d101.sh $t $a $u $sc
  echo "$(date +%T) w$w $t" >> $D/res/seq.log
done < $3
echo ALLDONE > $D/res/ALLDONE.w$w

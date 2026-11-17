#!/bin/bash
# seq93.sh WORKER JOBFILE: lines with (n % 1 == WORKER), one at a time in any free single lab slot
set -u
D=/opt/hs2lab/bh93; w=$1; n=0
while read -r t a u sc s; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % 1)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  SEED=${s:-86} /opt/hs2lab/labslot.sh --all $D/d93.sh $t $a $u $sc
  echo "$(date +%T) w$w $t" >> $D/res/seq.log
done < $2
echo ALLDONE > $D/res/ALLDONE.w$w

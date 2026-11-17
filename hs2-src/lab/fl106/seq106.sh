#!/bin/bash
# seq106.sh WORKER NW JOBFILE: job lines "TAG ARM SC", (n % NW == WORKER), one at a time in a free lab slot
set -u
D=/opt/hs2lab/fl106; w=$1; nw=$2; n=0
while read -r t a sc; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % nw)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  HSBIN=${HSBIN:-hs2-linux-amd64} /opt/hs2lab/labslot.sh bash $D/d106.sh $t $a $sc
  echo "$(date +%T) w$w $t" >> $D/res/seq.log
done < $3
echo ALLDONE > $D/res/ALLDONE.w$w

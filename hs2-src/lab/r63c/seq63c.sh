#!/bin/bash
# seq63c.sh WORKER NW JOBFILE (lab1): lines "TAG ARM SC", worker w takes every nw-th line; one labslot each.
set -u
D=/opt/hs2lab/r63c; R=${RS:-$D/res}; mkdir -p $R; w=$1; nw=$2; n=0
while read -r t a sc; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % nw)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  /opt/hs2lab/labslot.sh bash $D/d63c.sh $t $a $sc
  echo "$(date +%T) w$w $t" >> $R/seq.log
done < $3
echo ALLDONE > $R/ALLDONE.w$w

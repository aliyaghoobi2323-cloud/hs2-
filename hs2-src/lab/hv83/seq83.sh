#!/bin/bash
# seq83.sh JOBFILE: lines "TAG ARM N [SEED]" one by one on the whole lab
D=/opt/hs2lab/hv83
while read -r t a n s; do [ -z "$t" ] && continue; [ -e $D/stop ] && break
  SEED=${s:-83} /opt/hs2lab/labslot.sh --all $D/d83.sh $t $a $n
  echo "$(date +%T) $t" >> $D/res/seq.log
done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

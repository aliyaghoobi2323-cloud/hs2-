#!/bin/bash
# seq142.sh JOBFILE: lines "TAG ARM BG..." run one by one on the whole lab (labslot --all).
D=/opt/hs2lab/df142; mkdir -p $D/res
while read -r t a bg; do [ -z "$t" ] && continue; [ -e $D/stop ] && break
  /opt/hs2lab/labslot.sh --all $D/d142.sh $t $a "$bg"
  echo "$(date +%T) $t" >> $D/res/seq.log
done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

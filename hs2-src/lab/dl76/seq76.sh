#!/bin/bash
# seq76.sh JOBFILE: lines "TAG ARM MODE [VAR=val ...]" run one by one on the whole lab
D=/opt/hs2lab/dl76
while read -r t a m x; do [ -z "$t" ] && continue; [ -e $D/stop ] && break; env $x /opt/hs2lab/labslot.sh --all $D/d76.sh $t $a $m; echo "$(date +%T) $t" >> $D/res/seq.log; done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

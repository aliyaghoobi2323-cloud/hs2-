#!/bin/bash
# seq113.sh WORKER NW JOBFILE: lines "TAG SC EDGEBIN EXITBIN ENV(k=v,.. or -) VARS(k=v,.. or -)"
set -u
D=/opt/hs2lab/hs115; R=$D/res; w=$1; nw=$2; n=0
while read -r t sc hb hk ex vv; do n=$((n+1)); [ -z "${t:-}" ] && continue; [ $((n % nw)) -ne "$w" ] && continue; [ -e $D/stop ] && break
  V=""; [ "${vv:--}" != - ] && V="${vv//,/ }"
  env $V /opt/hs2lab/labslot.sh bash $D/d115.sh $t $sc $hb $hk ${ex:--}
  echo "$(date +%T) w$w $t" >> $R/seq.log
done < $3
echo ALLDONE > $R/ALLDONE.$(basename $3).w$w

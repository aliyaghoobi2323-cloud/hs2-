#!/bin/bash
# seq78.sh JOBFILE: lines "TAG ARM MODE" run one by one on the whole lab; a run
# whose iperf3 failed (e.g. "Address already in use" on --cport) is redone once
D=/opt/hs2lab/dl76
while read -r t a m x; do [ -z "$t" ] && continue; [ -e $D/stop ] && break
  for try in 1 2; do
    /opt/hs2lab/labslot.sh --all $D/d79.sh $t $a $m
    grep -q '"error"' $D/res/$t/heavy.json 2>/dev/null || break
    echo "$(date +%T) $t retry" >> $D/res/seq.log; sleep 20
  done
  echo "$(date +%T) $t" >> $D/res/seq.log
done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

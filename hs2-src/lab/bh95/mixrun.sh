#!/bin/bash
set -u
D=/opt/hs2lab/bh95
while [ ! -s $D/res/soakQS1/result.json ]; do sleep 5; done; sleep 3
while read -r t a u sc s; do SEED=$s /opt/hs2lab/labslot.sh $D/d95m.sh $t $a $u $sc; echo "$(date +%T) $t" >> $D/res/mix.log; done < $D/jm.txt
echo ALLDONE >> $D/res/mix.log

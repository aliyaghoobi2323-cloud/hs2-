#!/bin/bash
set -u
D=/opt/hs2lab/hs133
for w in 0 1 2; do bash $D/seq133.sh $w 3 $D/j133.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE" >> $D/res/seq.log

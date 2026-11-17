#!/bin/bash
# go132.sh: mixed runs (3 slots), then the tests on the idle host
set -u
D=/opt/hs2lab/hs132
for w in 0 1 2; do bash $D/seq132.sh $w 3 $D/jmix.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE mix" >> $D/res/seq.log
bash $D/tests132.sh

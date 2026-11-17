#!/bin/bash
set -u
D=/opt/hs2lab/hs131
while [ -e /proc/3514254 ]; do sleep 10; done
echo "$(date +%T) hs130runs done, start" >> $D/res/seq.log
for w in 0 1 2; do bash $D/seq131.sh $w 3 $D/j2.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE" >> $D/res/seq.log

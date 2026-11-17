#!/bin/bash
set -u
D=/opt/hs2lab/hs131
while systemctl is-active --quiet hs131runs; do sleep 10; done
echo "$(date +%T) knock-out done, start j3 (fixed k131 vs r63 off)" >> $D/res/seq.log
for w in 0 1 2; do bash $D/seq131.sh $w 3 $D/j3.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE j3" >> $D/res/seq.log

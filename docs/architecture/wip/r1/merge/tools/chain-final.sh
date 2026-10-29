#!/bin/bash
# chain-final.sh: after the suite chain, the final head blocks: make check + release, race + 386, rig RF
T=/root/rmerge/tools
L=/root/rmerge/chain-final.log
while kill -0 3810149 2>/dev/null; do sleep 10; done
echo "start $(date -u +%FT%TZ) pid $$ head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh check2 "R1 merge step 5: make check + release (final head)" 1500 bash -c "$T/blocks.sh check; $T/blocks.sh release" >> $L 2>&1
$T/lockrun.sh race386b "R1 merge step 5: race + 386 (final head)" 1500 $T/blocks.sh race386 >> $L 2>&1
$T/lockrun.sh RF "R1 merge step 5: rig N=1..600 + classes (final head)" 1500 env BUDGET=1440 $T/rigbody.sh RF /root/rmerge/plans/RF.txt 1 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

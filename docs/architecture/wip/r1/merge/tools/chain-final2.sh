#!/bin/bash
# chain-final2.sh: after RX and RY, the blocks of the head with the red team pass-2 fixes up to 1acf5ba
T=/root/rmerge/tools
L=/root/rmerge/chain-final2.log
while kill -0 94687 2>/dev/null; do sleep 10; done
echo "start $(date -u +%FT%TZ) pid $$ head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh check3 "R1 merge step 5: make check + release (head with red2 1acf5ba)" 1500 bash -c "$T/blocks.sh check; $T/blocks.sh release" >> $L 2>&1
$T/lockrun.sh race386c "R1 merge step 5: race + 386 (head with red2 1acf5ba)" 1500 env NO_UDP_AB=1 $T/blocks.sh race386 >> $L 2>&1
$T/lockrun.sh RF2 "R1 merge step 5: rig N=1..600 + classes (head with red2 1acf5ba)" 1500 env BUDGET=1400 $T/rigbody.sh RF2 /root/rmerge/plans/RF2.txt 1 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

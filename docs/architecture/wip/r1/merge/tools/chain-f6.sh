#!/bin/bash
T=/root/rmerge/tools
L=/root/rmerge/chain-f6.log
while kill -0 1884701 2>/dev/null; do sleep 10; done
echo "start $(date -u +%FT%TZ) head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh RF6 "R1 merge step 5: rig N=1..600 + classes + TUN, final code (CPUs 0-5)" 1500 env RIG_CPUS=0-5 BUDGET=1420 $T/rigbody.sh RF6 /root/rmerge/plans/RF6.txt 1 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

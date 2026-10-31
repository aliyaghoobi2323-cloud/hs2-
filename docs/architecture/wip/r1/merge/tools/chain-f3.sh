#!/bin/bash
# chain-f3.sh: after RU, the final head rig blocks RF3 (scale + classes) and RV3 (home A/B)
T=/root/rmerge/tools
L=/root/rmerge/chain-f3.log
while kill -0 1127656 2>/dev/null; do sleep 10; done
echo "start $(date -u +%FT%TZ) head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh RF3 "R1 merge step 5: rig N=1..600 + classes, final head (CPUs 0-5)" 1500 env RIG_CPUS=0-5 BUDGET=1400 $T/rigbody.sh RF3 /root/rmerge/plans/RF3.txt 1 >> $L 2>&1
$T/lockrun.sh RV3 "R1 merge step 5: rig home N=1,8 final head vs fin2 (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=/root/rmerge/plans/RV-bins.txt BUDGET=1420 $T/rigbody.sh RV3 /root/rmerge/plans/RV3.txt 4 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

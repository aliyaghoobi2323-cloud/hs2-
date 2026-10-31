#!/bin/bash
T=/root/rmerge/tools
L=/root/rmerge/chain-rv.log
echo "start $(date -u +%FT%TZ) head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh RV "R1 merge step 5: rig home N=1,8 merged head vs fin2 (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=/root/rmerge/plans/RV-bins.txt BUDGET=1420 $T/rigbody.sh RV /root/rmerge/plans/RV.txt 4 >> $L 2>&1
$T/lockrun.sh RV2 "R1 merge step 5: rig release binary N=1,600 (CPUs 0-5)" 900 env RIG_CPUS=0-5 BUDGET=800 $T/rigbody.sh RV2 /root/rmerge/plans/RV2.txt 1 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

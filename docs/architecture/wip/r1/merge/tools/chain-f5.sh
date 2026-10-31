#!/bin/bash
# chain-f5.sh: the rig blocks on the release binary still owed by step 5
T=/root/rmerge/tools
P=/root/rmerge/plans
L=/root/rmerge/chain-f5.log
echo "start $(date -u +%FT%TZ) head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> $L
$T/lockrun.sh RF5 "R1 merge step 5: rig home N=8 release binary vs fin2 (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=$P/RF5-bins.txt BUDGET=1400 $T/rigbody.sh RF5 $P/RF5.txt 4 >> $L 2>&1
$T/lockrun.sh RL "R1 merge step 5: rig every link carrying data (CPUs 0-5)" 1500 env RIG_CPUS=0-5 BUDGET=1400 $T/rigbody.sh RL $P/RL.txt 1 >> $L 2>&1
$T/lockrun.sh RQ "R1 merge step 5: rig newheavy with QUIET off (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=$P/RQ-bins.txt BUDGET=1400 $T/rigbody.sh RQ $P/RQ.txt 2 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

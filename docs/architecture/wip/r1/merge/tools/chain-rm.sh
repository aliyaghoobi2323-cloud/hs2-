#!/bin/bash
T=/root/rmerge/tools
PL=/root/rmerge/plans
L=/root/rmerge/chain-rm.log
while kill -0 1985569 2>/dev/null; do sleep 10; done
$T/lockrun.sh RM1 "R1 merge step 5: rig many active links, prod fin2 merged, N=64 300 (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=$PL/RM-bins.txt BUDGET=1420 $T/rigbody.sh RM1 $PL/RM1.txt 2 >> $L 2>&1
$T/lockrun.sh RM2 "R1 merge step 5: rig many active links, prod fin2 merged, N=600 (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=$PL/RM-bins.txt BUDGET=1420 $T/rigbody.sh RM2 $PL/RM2.txt 2 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

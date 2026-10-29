#!/bin/bash
# chain-rig.sh: the R1 merge rig blocks (scale smoke RS, class sanity RC)
T=/root/rmerge/tools
L=/root/rmerge/chain-rig.log
echo "start $(date -u +%FT%TZ) pid $$" >> $L
$T/lockrun.sh RS "R1 merge step 5: rig scale smoke N=1..600" 1500 $T/rigbody.sh RS /root/rmerge/plans/RS.txt 1 >> $L 2>&1
$T/lockrun.sh RC "R1 merge step 5: rig class sanity N=1,8" 1500 $T/rigbody.sh RC /root/rmerge/plans/RC.txt 1 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

#!/bin/bash
# chain-ru.sh: after chain-rv (RV2), the switch-isolation home block RU
T=/root/rmerge/tools
while kill -0 948928 2>/dev/null; do sleep 10; done
$T/lockrun.sh RU "R1 merge step 5: rig home N=8 which red2 switch (CPUs 0-5)" 1500 env RIG_CPUS=0-5 EXTRA_BINS=/root/rmerge/plans/RU-bins.txt BUDGET=1420 $T/rigbody.sh RU /root/rmerge/plans/RU.txt 4 >> /root/rmerge/chain-ru.log 2>&1

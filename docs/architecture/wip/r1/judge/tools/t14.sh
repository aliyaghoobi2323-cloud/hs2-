#!/bin/bash
# t14.sh: T14 (iii) of the R1 judge, outside the rig blocks: angle B with a
# data frame's Done before its write (as built) against after it (the judge
# lab switch HS2_LAB_LATE_DONE=1), one_vs_16 at N=1: fairness (b) and memory.
set -u
O=/root/r1-judge/t14
RS=/root/hs2-wt/r1-judge/docs/architecture/wip/rig-scale
mkdir -p $O
cat > $O/bins.txt <<EOF
b /root/r1-bins/hs2-b-b7f117f HS2_TUNE_HQG_SEGS=8
bl /root/r1-bins/hs2-b-b7f117f-lab HS2_TUNE_HQG_SEGS=8 HS2_LAB_LATE_DONE=1
EOF
{
  for b in b bl; do echo "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30 BIN=$b"; done
  for b in b bl; do echo "N=1 RATE=10 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30 BIN=$b"; done
} > $O/plan.txt
touch /root/r1-quiet.lock
trap 'rm -f /root/r1-quiet.lock' EXIT
REPS=3 RIG_ID=jt BINS=$O/bins.txt timeout 900 $RS/run.sh $O/plan.txt $O/out > $O/run.out 2>&1

#!/bin/bash
# mkb2.sh "VARENV": block 2 of the R1 judge (bins and plans), with the C3
# variant chosen in block 1. Labels: a, b (candidates); as, bs (C4-lite off,
# HS2_TUNE_SENDCAP=0); ar, br (the judge lab build: C4-lite floor
# min(1 MiB/s, 0.5 x max delivery 2 s), T3); old (the base binary).
set -u
V=${1:-}
cd /root/r1-judge
cat > bins-b2.txt <<EOF
a /root/r1-bins/hs2-a-481fa0a $V
b /root/r1-bins/hs2-b-b7f117f $V
as /root/r1-bins/hs2-a-481fa0a $V HS2_TUNE_SENDCAP=0
bs /root/r1-bins/hs2-b-b7f117f $V HS2_TUNE_SENDCAP=0
ar /root/r1-bins/hs2-a-481fa0a-lab $V HS2_LAB_RELFLOOR=1
br /root/r1-bins/hs2-b-b7f117f-lab $V HS2_LAB_RELFLOOR=1
EOF
c3() { for b in old a b; do echo "$1 BIN=$b"; done; }
c2() { for b in a b; do echo "$1 BIN=$b"; done; }
s5="MODE=fqcodel USERS=7 MIX=s5 DUR=20"
p10="RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 DUR=30"
{
  c3 "N=1 RATE=1000 RTT=72 LOSS=0.1 $s5"
  c3 "N=1 RATE=1000 RTT=72 LOSS=0 $s5"
  c3 "N=8 RATE=1000 RTT=72 LOSS=0 $s5"
} > plan-b2-solo.txt
{
  c3 "N=1 RATE=50 RTT=150 LOSS=0 $s5"
  c3 "N=1 $p10 MIX=lone16"
  c3 "N=1 $p10 MIX=tunping"
  c3 "N=8 RATE=20 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=tunping TUNV6=1 DUR=20"
  for b in a as ar b bs br; do echo "N=1 RATE=5 RTT=72 LOSS=0 $s5 BIN=$b"; done
  for b in a ar b br; do echo "N=1 RATE=1 RTT=72 LOSS=0 $s5 BIN=$b"; done
  c2 "N=300 $p10 MIX=one_vs_16"
} > plan-b2-r1.txt
{
  c3 "N=1 $p10 MIX=one_vs_16"
  c3 "N=1 $p10 MIX=newheavy"
  c3 "N=16 $p10 MIX=newheavy"
  c2 "N=16 $p10 MIX=one_vs_16"
  c2 "N=4 $p10 MIX=newheavy"
  c2 "N=64 $p10 MIX=newheavy"
  c2 "N=128 $p10 MIX=newheavy"
} > plan-b2-r2.txt
wc -l plan-b2-*.txt
cat bins-b2.txt

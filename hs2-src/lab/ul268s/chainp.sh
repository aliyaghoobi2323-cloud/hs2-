#!/bin/bash
# chainp.sh LAB_ID "n:bin:pdark ..." : parallel-lab chain (ul268i), 20 pps ping
cd /opt/hs2lab/ul268s; mkdir -p runs; ID=$1
for r in $2; do
  IFS=: read n b pd e <<< "$r"; [ -e STOP ] && break
  echo "$(date -u +%T) L$ID start $n $b PDARK=$pd $e" >> chainp.log
  O=/opt/hs2lab/ul268s/runs/$n SB=/opt/hs2lab/dl269/bin/$b TT=${TT:-140} SHARED_DN=2gbit BGRS=5_10_20_30_50_5_10_20_30_50 XENV="HS2_DL259_PATHQ=0 ${e//,/ }" KHX="HS2_MCONN_STRIPE_JUMP_MS=1500" DIRF= DIRN=up PDARK=$pd LAB_ID=$ID PING_I=0.05 bash h/dlab.sh
  echo "$(date -u +%T) L$ID end $n" >> chainp.log
done
echo "$(date -u +%T) L$ID chain done" >> chainp.log

#!/bin/bash
# ul268s ABAB: A=bin264 B=hs2_268s; PDARK=1 data-dark one path at a time; C*=no fault control
cd /opt/hs2lab/ul268s; mkdir -p runs
for r in ${RUNS:-A1:hs2_264:1 B1:hs2_268s:1 B2:hs2_268s:1 A2:hs2_264:1 CB:hs2_268s:0 CA:hs2_264:0 A3:hs2_264:1 B3:hs2_268s:1}; do
  IFS=: read n b pd <<< "$r"; [ -e STOP ] && break
  echo "$(date -u +%T) start $n $b PDARK=$pd" >> chain.log
  O=/opt/hs2lab/ul268s/runs/$n SB=/opt/hs2lab/dl269/bin/$b TT=${TT:-140} SHARED_DN=2gbit BGRS=5_10_20_30_50_5_10_20_30_50 XENV="HS2_DL259_PATHQ=0" KHX="HS2_MCONN_STRIPE_JUMP_MS=1500" DIRF= DIRN=up PDARK=$pd LAB_ID=30 bash h/dlab.sh
  echo "$(date -u +%T) end $n" >> chain.log
done
echo "$(date -u +%T) chain done" >> chain.log

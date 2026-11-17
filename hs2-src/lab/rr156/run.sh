#!/bin/bash
# rr156 lab3 (198.51.100.235): three arms in parallel labslot slots, same rep.sh as rr155
# (lab/rr155/rep.sh, TOPO146 production topology, 30 byte-verified 2 Mbit calls, crowd 3000, TT 1000 = 2 cycles).
#   R5 = rr155 (4b99bf43) both sides, HS2_MCONN_RESCUE_RST=1
#   R6 = rr156 both sides, HS2_MCONN_RESCUE_RST=1
#   C  = cd152 (ac353506) both sides, key off (control)
# Analysis: python3 /opt/hs2lab/rr155/an155.py runs/ARM ; python3 /opt/hs2lab/rr155/classify.py runs/ARM
R=/opt/hs2lab/rr155
mkdir -p /opt/hs2lab/rr156/runs
for a in "R5 $R/hs2-rr155 HS2_MCONN_RESCUE_RST=1" "R6 /opt/hs2lab/rr156/hs2-rr156 HS2_MCONN_RESCUE_RST=1" "C $R/hs2-cd152 HS2_X=0"; do
  set -- $a
  systemd-run --unit=rr156-$1 --collect --setenv=O=/opt/hs2lab/rr156/runs/$1 --setenv=SB=$2 --setenv=SK=$2 \
    --setenv=TT=1000 --setenv=CROWD=3000 --setenv=XENV=$3 /opt/hs2lab/labslot.sh bash $R/rep.sh
  sleep 8
done

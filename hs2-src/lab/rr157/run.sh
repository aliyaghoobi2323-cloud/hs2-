#!/bin/bash
# rr157 lab3 (198.51.100.235): two arms in parallel labslot slots 30 and 32 (slot 31, 6 cores, held by unit rr157-hold
# by a sleeper), rr157/rep.sh = lab/rr155/rep.sh with HS2_MCONN_TRACE off (TOPO146, 30 byte-verified 2 Mbit
# calls, crowd 3000, TT 1000 = 2 cycles, NBG 5).
#   B = rel65 (9d8389a2) both sides, key off;  N = rr157 both sides, HS2_MCONN_RESCUE_RST=1
D=/opt/hs2lab/rr157
mkdir -p $D/runs
for a in "B $D/hs2-rel65 HS2_X=0" "N $D/hs2-rr157 HS2_MCONN_RESCUE_RST=1"; do
  set -- $a
  systemd-run --unit=rr157-$1 --collect --setenv=O=$D/runs/$1 --setenv=SB=$2 --setenv=SK=$2 \
    --setenv=TT=1000 --setenv=CROWD=3000 --setenv=XENV=$3 /opt/hs2lab/labslot.sh bash $D/rep.sh
  sleep 8
done

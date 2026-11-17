#!/bin/bash
# rs271 one.sh NAME SB SK : env RAT CROWD LLN TT IRX KHX
L=/opt/hs2lab/rs271; n=$1; SB=$2; SK=$3
mkdir -p $L/$n; echo "$(date -u +%FT%TZ) start $n SB=$SB SK=$SK RAT=${RAT:-0}" >> $L/seq.log
env O=$L/$n SB=$SB SK=$SK XENV="HS2_MCONN_RESCUE_RST=1" IRX="${IRX:-}" KHX="${KHX:-}" TT=${TT:-200} RAT=${RAT:-0} CROWD=${CROWD:-3000} LLN=${LLN:-1000} bash /opt/hs2lab/labslot.sh bash $L/rs.sh > $L/$n.out 2>&1
echo "$(date -u +%FT%TZ) end $n rc=$?" >> $L/seq.log

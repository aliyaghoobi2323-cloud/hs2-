#!/bin/bash
# d106.sh TAG ARM SC (fl106: fast-flap penalty A/B; from bh101 d101.sh, 10 paths = 5 Iran IPs x 2 kharej)
#   ARM A: rel60 defaults   B: HS2_LB_FLAP_FAST=1 (kharej/exit)   BR: B + HS2_MCONN_RESCUE=1 both
#       C: B + HS2_LB_CUT_NONEW=1 (edge)   N: HS2_LB_CUT_NONEW=1 (edge) only   H: HS2_LB_DEAD_HYST=5 both sides
#   HSBIN=file in $F/bin (default hs2-linux-amd64)
#   SC F: path 49.2->53.1 dark (100 % drop) 25 s / clean 25 s from 30 s, 5 dark phases (to 255 s), T 330 s
#      bP: path 49.2->53.1 single blackhole 30..150 s, T 240 s     Qt: quiet (steady losses only), T 300 s
# Steady: 0.8 % on Iran IP E (54.1) + 2.5 % pair 51.1<-49.2. Crowd 3000 with 40 s churn (new placements).
set -u
TAG=$1; ARM=$2; SC=$3; F=/opt/hs2lab/fl106; L=$F/labsrc/lab
export P3_OUT=$F/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d106-$TAG; W=$WD${LAB_ID:-}
B=$F/bin/${HSBIN:-hs2-linux-amd64}; BIN=$B; export BIN_KH=$B; IRX="HS2_LB_REC_DEBUG=1"; KHX=$IRX
case $ARM in
  A) ;;
  B) KHX="$KHX HS2_LB_FLAP_FAST=1";;
  BR) KHX="$KHX HS2_LB_FLAP_FAST=1 HS2_MCONN_RESCUE=1"; IRX="$IRX HS2_MCONN_RESCUE=1";;
  C) KHX="$KHX HS2_LB_FLAP_FAST=1"; IRX="$IRX HS2_LB_CUT_NONEW=1";;
  N) IRX="$IRX HS2_LB_CUT_NONEW=1";;
  H) KHX="$KHX HS2_LB_DEAD_HYST=5"; IRX="$IRX HS2_LB_DEAD_HYST=5";;
  *) echo "bad arm $ARM"; exit 2;;
esac
case $SC in
  F) TT=330s; PEV="1:1:0.025"; t=30; while [ $t -lt 255 ]; do PEV="$PEV $t:3:1 $((t+25)):3:1d"; t=$((t+50)); done;;
  bP) TT=240s; PEV="1:1:0.025 30:3:1 150:3:1d";;
  Qt) TT=300s; PEV="1:1:0.025";;
  *) echo "bad sc $SC"; exit 2;;
esac
export T=$TT
echo "ARM=$ARM SC=$SC BIN=$BIN KHX=$KHX IRX=$IRX PEV=$PEV" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 0.3%" PATH_E_NETEM="loss random 0.8%"
export KH2=1 PAIR_EVENTS="$PEV" LOSS_EVENTS=""
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export HEAVY=down HEAVY_P=1
export PPROF=0 SPTBIN=/opt/hs2lab/cg86/spt PROBE_EXTRA="-crowdlife 40s -samplelog $P3_OUT/samples.txt"
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run86b.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM SC=$SC" > $P3_OUT/meta.txt
cp $W/lbtime.txt $P3_OUT/ 2>/dev/null; rm -rf "$W"

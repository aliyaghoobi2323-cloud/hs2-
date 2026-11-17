#!/bin/bash
# d107.sh TAG ARM SC (fl107: CUT_NONEW + DEAD_HYST A/B; copy of fl106 d106.sh, results in fl107/res)
#   ARM A: rel60 defaults (CUT_NONEW forced 0)   N: HS2_LB_CUT_NONEW=1 (edge)   H: HS2_LB_DEAD_HYST=5 both sides
#       X: no env keys (binary defaults; mixed-version runs)
#   HSBIN (edge) / HSBIN_KH (exit): file in fl107/bin, default hs2-c (fl106 binary 309bd54b)
#   SC F: flap 49.2->53.1 dark 25 s / clean 25 s x5, T 330 s   bP: single blackhole 30..150 s, T 240 s
#      Qt: quiet T 300 s   Mx: quiet + short blackhole 40..80 s, T 180 s
set -u
TAG=$1; ARM=$2; SC=$3; F=/opt/hs2lab/fl107; L=/opt/hs2lab/fl106/labsrc/lab
export P3_OUT=$F/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d107-$TAG; W=$WD${LAB_ID:-}
BIN=$F/bin/${HSBIN:-hs2-c}; export BIN_KH=$F/bin/${HSBIN_KH:-${HSBIN:-hs2-c}}; IRX="HS2_LB_REC_DEBUG=1"; KHX=$IRX
case $ARM in
  A) IRX="$IRX HS2_LB_CUT_NONEW=0";;
  N) IRX="$IRX HS2_LB_CUT_NONEW=1";;
  H) KHX="$KHX HS2_LB_DEAD_HYST=5"; IRX="$IRX HS2_LB_DEAD_HYST=5";;
  X) ;;
  *) echo "bad arm $ARM"; exit 2;;
esac
case $SC in
  F) TT=330s; PEV="1:1:0.025"; t=30; while [ $t -lt 255 ]; do PEV="$PEV $t:3:1 $((t+25)):3:1d"; t=$((t+50)); done;;
  bP) TT=240s; PEV="1:1:0.025 30:3:1 150:3:1d";;
  Qt) TT=300s; PEV="1:1:0.025";;
  Mx) TT=180s; PEV="1:1:0.025 40:3:1 80:3:1d";;
  *) echo "bad sc $SC"; exit 2;;
esac
export T=$TT
echo "ARM=$ARM SC=$SC BIN=$BIN BIN_KH=$BIN_KH KHX=$KHX IRX=$IRX PEV=$PEV" > $P3_OUT/env.txt
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

#!/bin/bash
# d112.sh TAG SC EDGEBIN EXITBIN [ENV k=v,k=v both sides or -]  (hs112: heavy stall through a dark path)
#   SC bS: blackhole 49.2->53.1 30..90 s, T 150 s   F: flap dark 25/clean 25 x5, T 330 s   bP: 30..150, T 240
#   env: CROWD (default 3000), HEAVY (down|up, default down), HEAVY_CTL (direct = iperf control over bare path)
set -u
TAG=$1; SC=$2; F=/opt/hs2lab/hs112; L=/opt/hs2lab/fl106/labsrc/lab; EX=${5:--}
export P3_OUT=$F/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d112-$TAG; W=$WD${LAB_ID:-}
BIN=$F/bin/$3; export BIN_KH=$F/bin/$4; IRX="HS2_LB_REC_DEBUG=1 HS2_MCONN_STRIPE_LOG=1"; KHX=$IRX
[ "$EX" != - ] && { IRX="$IRX ${EX//,/ }"; KHX="$KHX ${EX//,/ }"; }
case $SC in
  F) TT=330s; PEV="1:1:0.025"; t=30; while [ $t -lt 255 ]; do PEV="$PEV $t:3:1 $((t+25)):3:1d"; t=$((t+50)); done;;
  bP) TT=240s; PEV="1:1:0.025 30:3:1 150:3:1d";;
  bS) TT=150s; PEV="1:1:0.025 30:3:1 90:3:1d";;
  Q) TT=150s; PEV="1:1:0.025";;
  *) echo "bad sc $SC"; exit 2;;
esac
export T=$TT
echo "SC=$SC BIN=$BIN BIN_KH=$BIN_KH KHX=$KHX IRX=$IRX PEV=$PEV CROWD=${CROWD:-3000} HEAVY=${HEAVY:-down} CTL=${HEAVY_CTL:-}" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 0.3%" PATH_E_NETEM="loss random 0.8%"
export KH2=1 PAIR_EVENTS="$PEV" LOSS_EVENTS=""
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export HEAVY=${HEAVY:-down} HEAVY_P=1 HEAVY_CTL=${HEAVY_CTL:-}
export PPROF=0 SPTBIN=/opt/hs2lab/cg86/spt PROBE_EXTRA="-crowdlife 40s -samplelog $P3_OUT/samples.txt"
( for i in $(seq 1 200); do sleep 2; [ -d $P3_OUT ] || break; { date +%T; ip netns exec kh${LAB_ID:-} ss -tni dst 192.168.53.1 2>/dev/null | grep -A1 ESTAB | head -400; } >> $P3_OUT/ss53.txt; done ) & SSP=$!
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run86b.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
cp $W/lbtime.txt $P3_OUT/ 2>/dev/null; rm -rf "$W"
kill $SSP 2>/dev/null

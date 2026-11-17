#!/bin/bash
# d76.sh TAG ARM MODE(d1|d8): dl75's lab (reverse l3mtcp, 10 paths 90 ms, loss
# A: hs2D77 (gate74 + inert dl76/dl77 keys), B: floor 6 (dl76), C: floor 6 + queue gate (dl77); exit: rel58
# 0.2-2 %, crowd 3000+call+game, one iperf3 -R) plus per-second ss -ti of the
# exit's tunnel link sockets only (src 192.168/16, not the crowd's inner sockets)
# when SS=1, and for ARM T/TB, HS2_DL76_TRACE on both sides. B/C: B_IR/C_IR env.
set -u
TAG=$1; ARM=$2; MD=$3; D=/opt/hs2lab/dl76; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$D/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d77-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$D/hs2D77; export BIN_KH=$D/hs2R58
case $ARM in
  A) ;;
  T) IRX="HS2_DL76_TRACE=1"; KHX="HS2_DL76_TRACE=1";;
  B) IRX="HS2_STRIPE_HEAVY_FLOOR_MB=6";;
  C) IRX="HS2_STRIPE_HEAVY_FLOOR_MB=6 HS2_STRIPE_HEAVY_FLOOR_GATE=1";;
  H) IRX="HS2_STRIPE_HEAVY_FLOOR_MB=6 HS2_STRIPE_HEAVY_FLOOR_GATE=2";;
  S) IRX="HS2_STRIPE_HEAVY_FLOOR_MB=6 HS2_STRIPE_HEAVY_FLOOR_GATE=1 HS2_STRIPE_HEAVY_FLOOR_SOFT=1";;
  L) IRX="HS2_STRIPE_HEAVY_FLOOR_MB=6 HS2_STRIPE_HEAVY_FLOOR_GATE=1 HS2_STRIPE_HEAVY_FLOOR_SOFT=1 HS2_STRIPE_HEAVY_FLOOR_QUS=1500";;
  TB) IRX="HS2_DL76_TRACE=1 ${B_IR:-}"; KHX="HS2_DL76_TRACE=1 ${B_KH:-}";;
  O) BIN=$D/hs2O;;
esac
echo "ARM=$ARM BIN=$BIN IRX=$IRX KHX=$KHX" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; [ $MD = d8 ] && HP=8; [ $MD = u1 ] && HV=up
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
if [ "${SS:-0}" = 1 ]; then
( for i in $(seq 1 40); do sleep 1; echo "@T $(date +%s.%N)"; ip netns exec kh$LAB_ID ss -tinH state established '( src 192.168.0.0/16 )' 2>/dev/null; done > $P3_OUT/ss.txt ) &
SP=$!
fi
env BIN=$BIN BIN_KH=${BIN_KH:-$BIN} IR_ENV="$IRX" KH_ENV="$KHX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
[ "${SS:-0}" = 1 ] && kill $SP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
rm -rf $W

#!/bin/bash
# d83.sh TAG ARM N: dl79/sp82 lab (reverse l3mtcp, 10 paths 90 ms rtt, loss 0.2-2 %, crowd 3000+call+game)
# plus N simultaneous heavy users (mkusers.py, seed = SEED or 83) with churn. Edge bin79, exit rel58.
set -u
TAG=$1; ARM=$2; N=$3; D=/opt/hs2lab/hv83; L=$D/labsrc/lab
export P3_OUT=$D/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d83-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$D/hs2B79; export BIN_KH=$D/hs2R58
case $ARM in
  A) ;;
  W) IRX="HS2_SMUX_AUTOWIN_SHARE_MB=8"; KHX="HS2_SMUX_AUTOWIN_SHARE_MB=8";;
  L) IRX="HS2_PICK_LANE=8";;
  X0) BIN=$D/hs2X; BIN_KH=$D/hs2X;;
  XG) BIN=$D/hs2X; BIN_KH=$D/hs2X; IRX="HS2_LB_COMMON_GUARD=1"; KHX="HS2_LB_COMMON_GUARD=1";;
esac
export T=30s
if [ "$N" -gt 0 ]; then python3 $D/mkusers.py $N ${SEED:-83} 30 > $P3_OUT/users.txt; export HV_USERS=$P3_OUT/users.txt; fi
echo "ARM=$ARM N=$N BIN=$BIN IRX=$IRX KHX=$KHX SEED=${SEED:-83}" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 SPTBIN=$D/spt
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run83.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM N=$N" > $P3_OUT/meta.txt
rm -rf $W

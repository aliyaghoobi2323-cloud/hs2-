#!/bin/bash
# d61.sh TAG ARM MODE(d8|u1): rel61 quick A/B on d60's lab (10 paths 45 ms, loss 0.2-2 %,
# crowd 3000 + call + game, one iperf3). ARM (edge / exit):
#   P rel60/rel60  Q rel61/rel61  X rel61/rel58  Y rel58/rel61  M dl79/rel61 (staged rollout pairing)
#   R rel61/rel61 with HS2_MCONN_RESCUE=0 both sides
# Run via seq61.sh on labslot --all (lab3).
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/rel61; D=/opt/hs2lab/dl76; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/r61-$TAG; W=$WD$LAB_ID
XE=""
case $ARM in
  P) BIN=$R/hs2R60; BK=$R/hs2R60;;
  Q) BIN=$R/hs2R61; BK=$R/hs2R61;;
  X) BIN=$R/hs2R61; BK=$D/hs2R58;;
  Y) BIN=$D/hs2R58; BK=$R/hs2R61;;
  M) BIN=$D/hs2D79; BK=$R/hs2R61;;
  R) BIN=$R/hs2R61; BK=$R/hs2R61; XE=HS2_MCONN_RESCUE=0;;   # rel61 with RESCUE off both sides
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$BIN BIN_KH=$BK XE=$XE" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
env BIN=$BIN BIN_KH=$BK IR_ENV="$XE" KH_ENV="$XE" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
rm -rf $W

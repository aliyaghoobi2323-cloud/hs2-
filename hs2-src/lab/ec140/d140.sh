#!/bin/bash
# d140.sh TAG ARM MODE(d1|u1|q): echo-p99 attribution for rel62k2 (copy of rk139/d139.sh, lab1, hv138 labsrc).
# Exit rel61h2 in every arm; edge: H rel61h2, A rel62k2, A1 rel62k2 DL_FAST=0, A2 rel62k2 RETIRE_KEEP_IDLE=0,
# F rel62k3 (fix). MODE q = no heavy (crowd + call + game + echo only).
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/ec140; L=/opt/hs2lab/hv138/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/e140-$TAG; W=$WD$LAB_ID
XE=""; BK=$R/hs2R61H2
case $ARM in
  H) BIN=$R/hs2R61H2;;
  A) BIN=$R/hs2K2;;
  A1) BIN=$R/hs2K2; XE="HS2_MCONN_STRIPE_DL_FAST=0";;
  A2) BIN=$R/hs2K2; XE="HS2_RETIRE_KEEP_IDLE=0";;
  F) BIN=$R/hs2K3;;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$(sha256sum $BIN|cut -c1-8) BIN_KH=$(sha256sum $BK|cut -c1-8) XE=$XE" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in u1) HV=up;; q) HV="";; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
env BIN=$BIN BIN_KH=$BK IR_ENV="$XE" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
cp $W/heavy.json $P3_OUT/ 2>/dev/null
cp $W/ir.log $P3_OUT/ir.log 2>/dev/null; gzip -f $P3_OUT/ir.log 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E 'panic|fatal error' $W/$s.log 2>/dev/null)"; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
rm -rf $W

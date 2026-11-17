#!/bin/bash
# d116.sh TAG ARM MODE(d1|d8|q): dr116 HS2_MCONN_STRIPE_DL_FAST on the d60 (dl79 prod-like) lab, lab2.
# A hs2D both, key off; B hs2D both + HS2_MCONN_STRIPE_DL_FAST=1 on Iran (the edge decides every stripe growth).
# X1 Iran bin79 + Kharej hs2D; X2 Iran rel60 + Kharej hs2D; X3 Iran hs2D+key + Kharej rel60. q = no heavy (quiet control).
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/dr116; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/a116-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$R/hs2D; export BIN_KH=$R/hs2D
case $ARM in
  A) ;;
  B) IRX="HS2_MCONN_STRIPE_DL_FAST=1";;
  C) BIN=$R/hs2C; IRX="HS2_MCONN_STRIPE_DL_FAST=1";;
  L) BIN=$R/hs2L; IRX="HS2_MCONN_STRIPE_DL_FAST=1 HS2_MCONN_STRIPE_LOG=1";;
  X1) BIN=$R/hs2B79;;
  X2) BIN=/opt/hs2lab/ab114/hs2R60;;
  X3) IRX="HS2_MCONN_STRIPE_DL_FAST=1"; export BIN_KH=/opt/hs2lab/ab114/hs2R60;;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$BIN IRX=$IRX KHX=$KHX" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; q) HV="";; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
( for i in $(seq 1 80); do sleep 0.5; f=$(ls /run/hs2/tmp-a116-$TAG*-ir.json.status.json 2>/dev/null|head -1); [ -n "$f" ] && echo "$(date +%s.%N) $(tr -d '\n' < $f | grep -o '"[a-z_]*\(floor\|stripe\|cut\|rescue\)[a-z_]*": *[0-9]*' | tr '\n' ' ')"; done > $P3_OUT/stat.txt ) &
FP=$!
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
kill $FP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
grep -c "Segmentation fault" $P3_OUT/run.err > $P3_OUT/segv.txt
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
for f in $P3_OUT/*.log; do [ -f "$f" ] && gzip -6 "$f"; done
rm -rf $W

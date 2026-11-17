#!/bin/bash
# d111.sh TAG ARM MODE(d1|d8): rel61 vs rel60 download on the d60 (dl79 prod-like) lab, lab2.
# A rel60 both; B rel61 both; C rel61 + HS2_LB_CUT_NONEW=0 on edge (Iran); D rel61 + HS2_MCONN_RESCUE=0 both.
# Run via seq111.sh (labslot --all). Same lab params as rel60/d60.sh. Logs kept gzipped (low-run analysis).
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/ab111; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/a111-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$R/hs2R61; export BIN_KH=$R/hs2R61
case $ARM in
  A) BIN=$R/hs2R60; export BIN_KH=$R/hs2R60;;
  B) ;;
  C) IRX="HS2_LB_CUT_NONEW=0";;
  D) IRX="HS2_MCONN_RESCUE=0"; KHX="HS2_MCONN_RESCUE=0";;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$BIN IRX=$IRX KHX=$KHX" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
( for i in $(seq 1 80); do sleep 0.5; f=$(ls /run/hs2/tmp-a111-$TAG*-ir.json.status.json 2>/dev/null|head -1); [ -n "$f" ] && echo "$(date +%s.%N) $(tr -d '\n' < $f | grep -o '"[a-z_]*\(floor\|stripe\|cut\|rescue\)[a-z_]*": *[0-9]*' | tr '\n' ' ')"; done > $P3_OUT/stat.txt ) &
FP=$!
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
kill $FP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
grep -c "Segmentation fault" $P3_OUT/run.err > $P3_OUT/segv.txt
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
for f in $P3_OUT/*.log; do [ -f "$f" ] && gzip -6 "$f"; done
rm -rf $W

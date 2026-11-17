#!/bin/bash
# d62.sh TAG ARM MODE(d1|d8|u1|q): rel62 acceptance on the d118 (dl79 prod-like) lab, lab2.
# G0: rel62 defaults (DL_FAST on edge, RES_GUARD off); G1: rel62 + RES_GUARD=1 on edge; R61: rel61 both sides; ME: rel62 edge + rel61 exit; MX: rel61 edge + rel62 exit.
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/rel62; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/a62-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$R/hs2R62; export BIN_KH=$R/hs2R62
case $ARM in
  G0) ;;
  G1) IRX="HS2_MCONN_STRIPE_RES_GUARD=1";;
  R61) BIN=$R/hs2R61; export BIN_KH=$R/hs2R61;;
  ME) export BIN_KH=$R/hs2R61;;
  MX) BIN=$R/hs2R61;;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$BIN IRX=$IRX KHX=$KHX" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; q) HV="";; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
( for i in $(seq 1 80); do sleep 0.5; f=$(ls /run/hs2/tmp-a62-$TAG*-ir.json.status.json 2>/dev/null|head -1); [ -n "$f" ] && echo "$(date +%s.%N) $(tr -d '\n' < $f | grep -o '"[a-z_]*\(floor\|stripe\|cut\|rescue\)[a-z_]*": *[0-9]*' | tr '\n' ' ')"; done > $P3_OUT/stat.txt ) &
FP=$!
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
kill $FP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
grep -c "Segmentation fault" $P3_OUT/run.err > $P3_OUT/segv.txt
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
for f in $P3_OUT/*.log; do [ -f "$f" ] && gzip -6 "$f"; done
rm -rf $W

#!/bin/bash
# d142.sh TAG ARM "BG" (df142 A/B, copy of cs141/d141.sh): cs141 single test download among background users (run141.sh, d139 lab: 10 paths
# 45 ms, loss 0.2-2 %, crowd 3000 + call + game). BG e.g. "d:150 u:40 d:60" (dir:Mbit/s, one iperf3 each,
# from t=0 for BG_SECS); the test download (iperf3 -R P1, 12 s) starts HEAVY_DELAY=8 s later.
#   K rel62k2 edge (prod 5d95d5c7)   N rel62k3 (DL_FAIR default on, no env)   (exit rel61h2 both)
set -u
TAG=$1; ARM=$2; BGS=$3; R=/opt/hs2lab/df142; L=/opt/hs2lab/lbv81/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/h142-$TAG; W=$WD$LAB_ID; IRE=""
case $ARM in
  K) BIN=$R/hs2K2b;;
  N) BIN=$R/hs2D3;;   # rel62k3: DL_FAIR on by default
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
BK=$R/hs2R61H2
echo "ARM=$ARM BIN=$(sha256sum $BIN|cut -c1-8) BIN_KH=$(sha256sum $BK|cut -c1-8) IRE=$IRE BG=$BGS" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=26s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=9 BG_SECS=23 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct IR_ENV="$IRE"
env BIN=$BIN BIN_KH=$BK HEAVY=down HEAVY_P=1 LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash /opt/hs2lab/df142/run141.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
cp $W/heavy.json $W/hstart $W/bgstart $W/bg*.json $P3_OUT/ 2>/dev/null
grep -a -E 'download stripe at|striped over|stripe pick|dlfair' $W/ir.log > $P3_OUT/stripe.log 2>/dev/null
cp /run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json $P3_OUT/irstatus.json 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E 'panic|fatal error' $W/$s.log 2>/dev/null)"; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
rm -rf $W

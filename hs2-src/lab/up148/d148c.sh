#!/bin/bash
# d148.sh TAG ARM SC (up148: HS2_LB_UPLOSS on the edge). 5 paths 45 ms, base loss 0.2-0.5 %.
#  ARM OFF: no key   ON: IR_ENV HS2_LB_UPLOSS=1 (same binary both arms; exit unchanged)
#  SC U: path 1 (192.168.51.x) 3 % loss Iran->kharej only   D: path 1 3 % kharej->Iran only   N: no extra loss
#  6 upload streams (iperf3 -P 6) 45..85 s; light (ping hs0, call, game, echo) from 40 s; crowd 1000
set -u
TAG=$1; ARM=$2; SC=$3; R=/opt/hs2lab/up148
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/u148-$TAG; W=$WD$LAB_ID; IRE=""
case $ARM in OFF) IRE="";; ON) IRE="HS2_LB_UPLOSS=1";; *) exit 2;; esac
unset UPLOSS_PATH DNLOSS_PATH
HX=""; HD=45; HS=40; HPP=6
case $SC in L) export UPLOSS_PATH=1 UPLOSS_PCT=3%; HX="-b 4M"; HD=0; HS=88; HPP=4;; M) HX="-b 4M"; HD=0; HS=88; HPP=4;; U) export UPLOSS_PATH=1 UPLOSS_PCT=3%;; D) export DNLOSS_PATH=1 DNLOSS_PCT=3%;; N) ;; *) exit 2;; esac
echo "ARM=$ARM SC=$SC IRE=$IRE LAB_ID=$LAB_ID" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 0.3%" PATH_D_NETEM="loss random 0.4%" PATH_E_NETEM="loss random 0.3%"
export T=90s LATE=40s HEAVY_SECS=$HS HEAVY_OMIT=0 HEAVY_DELAY=$HD HEAVY_XARGS="$HX" BG="" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=1000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 IR_ENV="$IRE"
( for i in $(seq 1 60); do echo "T $(date +%s.%N)"; ip netns exec mid$LAB_ID tc -s qdisc show 2>/dev/null; sleep 3; done ) > $P3_OUT/qd.txt 2>&1 & QS=$!
env BIN=$R/bin/hs2 BIN_KH=$R/bin/hs2 HEAVY=up HEAVY_P=$HPP LOGDIR=$P3_OUT RUNTAG=$TAG timeout 400 bash $R/run148.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
kill $QS 2>/dev/null
cp $W/heavy.json $W/emu-qdisc.txt $P3_OUT/ 2>/dev/null
grep -a -E "lb auto2" $W/ir.log > $P3_OUT/ir-lb.log 2>/dev/null
grep -a -E "lb auto2" $W/kh.log > $P3_OUT/kh-lb.log 2>/dev/null
cp /run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json $P3_OUT/irstatus.json 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E "panic|fatal error" $W/$s.log 2>/dev/null)"; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
rm -rf $W

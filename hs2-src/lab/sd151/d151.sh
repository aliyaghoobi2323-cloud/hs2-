#!/bin/bash
# d151.sh TAG ARM (sd151: the steering's asked-dial spin). 5 paths 45 ms, path 1 3 % upload loss
# (the upload judge cuts it). Both arms: HS2_LB_UPLOSS=1 HS2_LB_STRIPE_STEER=1 (Iran), STRIPE_STEER=1 (exit).
#  ARM BASE: st150 binary (+ the log line on asks only)   NEW: sd151 binary. HP upload streams from 40 s for HT s; light probes from 40 s; crowd 1000
set -u
TAG=$1; ARM=$2; R=/opt/hs2lab/sd151
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/s151-$TAG; W=$WD$LAB_ID
case $ARM in BASE) B=$R/bin/hs2-base2;; NEW) B=$R/bin/hs2-new2;; NEW3) B=$R/bin/hs2-new3;; *) exit 2;; esac
HT=${HT:-75}; HP=${HP:-6}; TT=$((HT + 45)); MXL=${MXL:-150}; MNL=${MNL:-100}
export UPLOSS_PATH=1 UPLOSS_PCT=3%
echo "ARM=$ARM SC=U HT=$HT HP=$HP MXL=$MXL LAB_ID=$LAB_ID" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":$MXL,\"min_links\":$MNL,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 0.3%" PATH_D_NETEM="loss random 0.4%" PATH_E_NETEM="loss random 0.3%"
export T=${TT}s LATE=40s HEAVY_SECS=$HT HEAVY_OMIT=0 HEAVY_DELAY=40 HEAVY_XARGS="" BG="" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=$MNL MAX_LINKS=$MXL CROWD=1000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 IR_ENV="HS2_LB_UPLOSS=1 HS2_LB_STRIPE_STEER=1 HS2_LB_STRIPE_STEER_LOG=1" KH_ENV="HS2_LB_STRIPE_STEER=1"
( for i in $(seq 1 $((TT / 3 + 20))); do echo "T $(date +%s.%N)"; ip netns exec mid$LAB_ID tc -s qdisc show 2>/dev/null; sleep 3; done ) > $P3_OUT/qd.txt 2>&1 & QS=$!
env BIN=$B BIN_KH=$B HEAVY=up HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG timeout $((TT + 300)) bash $R/run148.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
kill $QS 2>/dev/null
cp $W/heavy.json $W/emu-qdisc.txt $P3_OUT/ 2>/dev/null
grep -a -E "lb auto2" $W/ir.log > $P3_OUT/ir-lb.log 2>/dev/null
grep -a -E "lb auto2" $W/kh.log > $P3_OUT/kh-lb.log 2>/dev/null
grep -a -E "stripe steer" $W/ir.log > $P3_OUT/ir-steer.log 2>/dev/null
grep -a -E "stripe steer" $W/kh.log > $P3_OUT/kh-steer.log 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E "panic|fatal error" $W/$s.log 2>/dev/null)"; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
rm -rf $W

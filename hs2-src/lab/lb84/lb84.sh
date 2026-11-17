#!/bin/bash
# lb84.sh TAG ARMKEYS: short (330 s) LB A/B run, one lab slot, build $HS2N both sides.
# 4 paths rtt ~90 ms, base loss a..d = 0.1 0.2 0.3 0.8 % (d = the steady 0.8 % path, F1),
# event: path a 10 % loss t=40..260 (lowered, cut, cut-drained, cutDrainForce at cut+60 s: F3),
# crowd 1500 (light, churn 40 s), 2 Mbit call, game, heavy down x4 30 s at t=270.
set -u
TAG=$1; D=/opt/hs2lab/lb84; L=/opt/hs2lab/lbv81/labsrc/lab
EB=${HS2E:-/opt/hs2lab/lbv81/hs2N}; XB=${HS2X:-/opt/hs2lab/lbv81/hs2N}
export P3_OUT=$D/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/lb84-$TAG; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"
echo "IRE=$IRE KHE=$KHE edge=$(sha256sum $EB|cut -c1-8) exit=$(sha256sum $XB|cut -c1-8) lab=$LAB_ID" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":120,\"min_links\":80,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0
J="1500us 25% distribution normal"; JU=${J// /_}
B=(0.1 0.2 0.3 0.8); X=(A B C D)
for k in 0 1 2 3; do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
EV="${EVENTS:-40:0:delay_45ms_${JU}_loss_random_10% 260:0:delay_45ms_${JU}_loss_random_0.1%}"
export NETEM_EVENTS="$EV"; echo "$EV" > $P3_OUT/netem-plan.txt
export T=${TT:-330s} HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=80 MAX_LINKS=120 CROWD=${CROWD_N:-1500} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=4 LB_MODE=auto LOSS=0 KILL_AT=0 LB_SAMPLE=1
export PPROF=0 PROBE_EXTRA="-crowdlife ${CLIFE:-40s} -samplelog $P3_OUT/samples.txt"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $P3_OUT/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  while [ $(el) -lt ${HVAT:-270} ]; do sleep 1; done
  ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 30 -R -J > $P3_OUT/hv-d.json 2>&1 ) > /dev/null 2>&1 &
env BIN=$EB BIN_KH=$XB IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
sleep 3
cp $W/ir.log $W/kh.log $W/netemevents.txt $W/lbsamp.jsonl $W/ping.txt $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=$LAB_ID done $(date)" > $P3_OUT/meta.txt

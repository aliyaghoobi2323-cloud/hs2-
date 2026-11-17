#!/bin/bash
# wj178.sh TAG: one ~10 min LB run (production code defaults, ARM="KEY=V ..."
# on both sides). 6 paths reverse l3mtcp, rtt ~90 ms, base loss 0.1-0.5 %;
#   path c (k2): loss bursts 7 % for 25 s every 90 s from t=60 (cut/recover cycles: A)
#   path a (k0): dark (100 %) 15 s at t=150 and t=450; path e (k4) dark 15 s at t=300 (real stuck calls: B)
# Load: crowd 1500 (light p99), 12 echo calls (6 with talk spurts, callers_ts.py), game,
# heavy iperf3 down x4 40 s every 100 s from t=40.
# Run: systemd-run --unit=wj178-TAG --collect /opt/hs2lab/labslot.sh bash /opt/hs2lab/wj178/wj178.sh TAG
set -u
TAG=$1; D=/opt/hs2lab/wj178; L=/opt/hs2lab/rs146/labsrc/lab; EB=${EB:-$D/hs2-new}
O=$D/res/$TAG; rm -rf $O; mkdir -p $O
WD=/tmp/wj178-$TAG; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"
echo "IRE=$IRE KHE=$KHE bin=$(sha256sum $EB|cut -c1-8) lab=$ID" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":120,\"min_links\":80,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=400mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0
J="1500us 25% distribution normal"; JU=${J// /_}
B=(0.1 0.3 0.2 0.5 0.1 0.3); X=(A B C D E F)
for k in $(seq 0 5); do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
base(){ echo "delay_45ms_${JU}_loss_random_${B[$1]}%"; }
EV=""
for t in 60 150 240 330 420 510; do EV="$EV $t:2:delay_45ms_${JU}_loss_random_7% $((t+25)):2:$(base 2)"; done
EV="$EV 150:0:delay_45ms_loss_random_100% 165:0:$(base 0) 450:0:delay_45ms_loss_random_100% 465:0:$(base 0)"
EV="$EV 300:4:delay_45ms_loss_random_100% 315:4:$(base 4)"
export NETEM_EVENTS="$EV"; echo "$EV" | tr ' ' '\n' > $O/netem-plan.txt
export T=620s HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 GAME=1 MIN_LINKS=80 MAX_LINKS=120 CROWD=1500 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=6 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  ( ip netns exec ir$ID env NSPURT=6 python3 $D/callers_ts.py 127.0.0.1:8443 12 $O/calls.jsonl $O/t0 & echo $! > $O/callers.pid; wait ) &
  ( t=40; while [ $t -lt 600 ]; do while [ $(el) -lt $t ]; do sleep 1; done
      ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 40 -R -J > $O/hv-d-$t.json 2>&1
      t=$((t+100)); done ) &
  wait ) > /dev/null 2>&1 &
env BIN=$EB BIN_KH=$EB HEAVY=down HEAVY_P=1 IR_ENV="$IRE" KH_ENV="$KHE" LOGDIR=$O RUNTAG=$TAG bash $L/run.sh > $O/run.out 2> $O/run.err
kill $(cat $O/callers.pid) 2>/dev/null; sleep 3
cp $W/ir.log $W/kh.log $W/netemevents.txt $W/ping.txt $O/ 2>/dev/null
tail -1 $O/run.out > $O/result.json
echo "LAB_ID=${LAB_ID:-} done $(date)" > $O/meta.txt

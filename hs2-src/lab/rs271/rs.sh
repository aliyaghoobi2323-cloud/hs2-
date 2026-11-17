#!/bin/bash
# rs271: kharej-only restart under crowd + long-lived conns (from ab267/qc.sh). env RAT (s after start, 0 = no restart),
# CROWD [3000], LLN [1000], LLP [30], TT [200]. Port 8451 -> kh 5211 carries ll.py.
set -u
H=/opt/hs2lab/r68ft/h; O=${O:?}; L=/opt/hs2lab/rs271
SB=${SB:?}; SK=${SK:-$SB}; TT=${TT:-200}
export P3_OUT=$O; mkdir -p $O/logs $O/tests
WD=/tmp/rs271w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 ${XENV:-} ${IRX:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${XENV:-} ${KHX:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT CROWD=${CROWD:-3000} RAT=${RAT:-0} LLN=${LLN:-1000} start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":200,\"min_links\":150,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=${DLY:-37ms} QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"
B=(${LOSSB:-0.1_0.3_0.2_0.5_0.1_0.3_0.2_0.8_0.4_0.2}); B=(${B[@]//_/ })
X=(A B C D E F G H I J)
for k in $(seq 0 9); do export PATH_${X[$k]}_NETEM="delay ${DLY:-37ms} $J loss random ${B[$k]}%"; done
export NETEM_EVENTS=""
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp $H/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="d:1" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  for p in $(lpid 5211); do kill $p; done; sleep 0.5
  ip netns exec kh$ID python3 $L/ll.py server 5211 > $O/ll_s.err 2>&1 & echo $! > $O/lls.pid
  sleep 0.5
  ip netns exec ir$ID python3 $L/ll.py client 127.0.0.1 8451 ${LLN:-1000} ${LLP:-30} $((TT-25)) $O/ll.json > $O/ll_c.err 2>&1 &
  if [ "${RAT:-0}" != 0 ]; then
    while [ $(( $(date +%s) - t0 )) -lt $RAT ]; do sleep 1; done
    KP=""; for p in $(pidof hs2kh); do [ "$(ip netns identify $p 2>/dev/null)" = kh$ID ] && KP=$p; done
    if [ -n "$KP" ]; then
      cp /proc/$KP/environ $O/kh.environ; cp /proc/$KP/cmdline $O/kh.cmdline
      echo "$(date +%s.%N) SIGTERM $KP" >> $O/restart.txt; kill -TERM $KP
      for i in $(seq 1 100); do [ -e /proc/$KP ] || break; sleep 0.1; done
      echo "$(date +%s.%N) exited $KP alive=$(test -e /proc/$KP && echo 1 || echo 0)" >> $O/restart.txt
      ip netns exec kh$ID python3 $L/khrestart.py $O/kh.environ $O/kh.cmdline $W >> $W/kh.log 2>&1 &
      NP=$!; echo $NP >> $W/pids; echo "$(date +%s.%N) started $NP" >> $O/restart.txt
    else echo "no kh pid" >> $O/restart.txt; fi
  fi
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rs271 bash $H/run141.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 8
[ -s $O/lls.pid ] && kill $(cat $O/lls.pid) 2>/dev/null  # the netns is gone by now: kill the ll server by its PID
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
tail -1 $O/run.out > $O/result.json
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -c -E 'panic|fatal error' > $O/$s.panics; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -E 'mconn rescue:' > $O/$s.rescue.txt; done
sleep 2; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
rm -rf $W

#!/bin/bash
# sc179.sh (lab3): storm-focused scale run, box not saturated.
# probe crowd 3000 (canary) + gen crowd 3000 + NBG heavy; storms from t=100:
# STN conns in STB every 25 s, hold 8 s. Profiles when PROF=1 (goroutine dumps in bursts).
set -u
D=/opt/hs2lab/sc161; K=/opt/hs2lab/lk179
O=${O:?}; SB=${SB:?}; SK=${SK:-$SB}; TT=${TT:-300}; NBG=${NBG:-2}; STN=${STN:-3000}; STB=${STB:-1500ms}
export P3_OUT=$O; mkdir -p $O/logs $O/tests $O/pp
WD=/tmp/sc179w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 ${IRX:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${KHX:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG STN=$STN STB=$STB start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":300,\"min_links\":150,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2); X=(A B C D E F G H I J)
for k in $(seq 0 9); do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
export NETEM_EVENTS=""
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp /opt/hs2lab/rr158/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=300 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  cpu(){ awk '{print $14+$15}' /proc/$1/stat 2>/dev/null || echo 0; }
  ( declare -A PC; lt=$(date +%s.%N)
    while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      now=$(date +%s.%N); dt=$(python3 -c "print(max(0.1,$now-$lt))"); lt=$now
      for s in "ir hs2" "kh hs2kh" "ir probe" "ir lk179gen"; do set -- $s
        for p in $(hpid $1 $2); do
          c=$(cpu $p); o=${PC[$p]:-$c}; PC[$p]=$c; pc=$(python3 -c "print(round(($c-$o)/$dt,0))")
          echo "$t $1 $2 pid=$p cpu%=$pc" >> $O/proc.txt; done; done
      echo "$t $(cat /proc/loadavg)" >> $O/load.txt
      sleep 5; done ) &
  gor(){ for s in ir kh; do ip netns exec $s$ID curl -s -m 10 "127.0.0.1:6060/debug/pprof/goroutine?debug=1" | head -c 2000000 > $O/pp/$1-$s.gor & done; wait; }
  if [ "${PROF:-0}" = 1 ]; then
  ( while [ $(el) -lt 60 ]; do sleep 1; done
    for s in ir kh; do ip netns exec $s$ID curl -s -m 40 "127.0.0.1:6060/debug/pprof/profile?seconds=20" > $O/pp/S1-$s.cpu & done; wait
    while [ $(el) -lt 113 ]; do sleep 0.2; done
    for s in ir kh; do ip netns exec $s$ID curl -s -m 40 "127.0.0.1:6060/debug/pprof/profile?seconds=20" > $O/pp/ST-$s.cpu &
      ip netns exec $s$ID curl -s -m 10 "127.0.0.1:6060/debug/pprof/mutex" > $O/pp/ST-$s.mutex0
      ip netns exec $s$ID curl -s -m 10 "127.0.0.1:6060/debug/pprof/block" > $O/pp/ST-$s.block0; done
    sleep 21
    for s in ir kh; do ip netns exec $s$ID curl -s -m 10 "127.0.0.1:6060/debug/pprof/mutex" > $O/pp/ST-$s.mutex1
      ip netns exec $s$ID curl -s -m 10 "127.0.0.1:6060/debug/pprof/block" > $O/pp/ST-$s.block1; done
    for b in 140 165; do
      while [ $(el) -lt $b ]; do sleep 0.05; done; sleep 0.3; gor b$b-03; sleep 0.4; gor b$b-08; sleep 0.6; gor b$b-15; done
    sleep 3; gor quiet ) &
  fi
  G=$K/lk179gen
  gen(){ at=$1; shift; while [ $(el) -lt $at ]; do sleep 0.05; done; [ -e $O/STOPS ] && return
    echo "$(el) gen $*" >> $O/actions.txt
    ip netns exec ir$ID $G "$@" >> $O/gen.txt 2>> $O/gen.err; }
  gen 20 -mode drop -n ${DN:-6000} -life 600s -every 700ms -frac 0.5 -reburst 2s -dropat 95s,145s -t $((TT-40))s -tag drop -win 2s &
  gen 90 -mode storm -n $STN -burst $STB -period 50s -hold 8s -t 60s -tag storm -win 2s &
  bgslot(){ i=$1; port=$((8451+i)); RANDOM=$((142+i*7)); sleep $((i*3))
    while [ ! -e $O/STOPS ] && [ $(el) -lt $((TT-60)) ]; do
      RS=(20 30 50 80 100); r=${RS[$((RANDOM % 5))]}; d=$((30 + RANDOM % 61)); dir=d; R=-R
      [ $((RANDOM % 10)) -ge 7 ] && { dir=u; R=""; }
      [ $(( $(el) + d )) -gt $((TT-30)) ] && d=$((TT - 30 - $(el))); [ $d -lt 10 ] && break
      s=$(el); timeout -k 5 $((d+25)) ip netns exec ir$ID iperf3 -c 127.0.0.1 -p $port --connect-timeout 5000 -t $d -b ${r}M $R -J > $O/bg$i.tmp 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/bg$i.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$s $(el) slot=$i dir=$dir rate=$r dur=$d mbit=$v rc=$rc" >> $O/bg.txt
      sleep $((5 + RANDOM % 16)); done; rm -f $O/bg$i.tmp; }
  for i in $(seq 0 $((NBG-1))); do bgslot $i & done
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=sc179 bash /opt/hs2lab/lk165/run141.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -c -E 'panic|fatal error' > $O/$s.panics; done
tail -1 $O/run.out > $O/result.json
sleep 5; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
for p in $(pidof iperf3); do n=$(ip netns identify $p 2>/dev/null); [ "$n" = ir$ID ] || [ "$n" = kh$ID ] || continue
  ls -l /proc/$p/fd 2>/dev/null | grep -q /opt/hs2lab/lab && kill $p; done
for p in $(ip netns pids ir$ID 2>/dev/null); do [ "$(cat /proc/$p/comm 2>/dev/null)" = lk179gen ] && kill $p; done
rm -rf $W

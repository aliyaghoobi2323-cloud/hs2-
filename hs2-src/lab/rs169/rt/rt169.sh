#!/bin/bash
# rt169.sh: rs169 retry-heavy lab (rr166 rep.sh adapted). Production topology TOPO146 (6 Iran IPv4 x 2 kharej
# IPv4 + IPv6 = 13 paths), crowd 3000 (life 40 s), call 2 mbit, game, NBG iperf3 background users, a test
# download every 120 s, 300 idle conns at t30, 30 BYTE-CHECKED calls (rr155 callers.py: CORRUPT on any lost /
# duplicated / reordered byte). No NETEM_EVENTS: ev169.py drives the outages (IP dark + reactive cascade on the
# first rescue join, every 40 s). XENV (HS2_MCONN_RESCUE_IDEM=0|1) on both sides. Env: O SB SK TT XENV TR.
set -u
R=/opt/hs2lab/rel64; D=/opt/hs2lab/rs169
O=${O:?}; L=/opt/hs2lab/rs146/labsrc/lab
SB=${SB:?}; SK=${SK:?}; TT=${TT:-480}; NBG=${NBG:-5}
mkdir -p $O/logs $O/tests
WD=$O/w; W=$WD$LAB_ID; ID=$LAB_ID
TR=${TR:-"HS2_PPROF=127.0.0.1:6060 HS2_MCONN_RESCUE_TRACE=1 HS2_END_LOG=1"}
IRE="$TR ${XENV:-}"; KHE="$TR ${XENV:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID cpus=$(taskset -pc $$|awk '{print $NF}')" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":260,\"min_links\":200,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1 TOPO146=1 KEEP_BACKLOG=1
J="1500us 25% distribution normal"
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2 0.3 0.1 0.4)
XL=(A B C D E F G H I J K L M)
for k in $(seq 0 12); do export PATH_${XL[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
unset NETEM_EVENTS
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp $R/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=200 MAX_LINKS=260 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=13 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 240); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  ksrv(){ for p in $(lpid $1); do kill $p 2>/dev/null; done; sleep 0.3; for p in $(lpid $1); do kill -9 $p 2>/dev/null; done
    ( for f in $(ls /proc/$BASHPID/fd); do [ $f -gt 2 ] && eval "exec $f>&-"; done
      exec ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p $1 >/dev/null 2>&1 ) &
    local w; for w in $(seq 1 25); do ip netns exec kh$ID ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  python3 $D/rt/ev169.py $W mid$ID $O/t0 $TT $O/events.jsonl > $O/ev.err 2>&1 &
  ( declare -A OFF=([ir]=0 [kh]=0)
    while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || { echo "$t $1 MISSING" >> $O/proc.txt; continue; }
        echo "$t $1 pid=$p rss_kb=$(awk '/VmRSS/{print $2}' /proc/$p/status) fds=$(ls /proc/$p/fd | wc -l)" >> $O/proc.txt
        lg=$W/$1.log; [ -e $lg ] || continue
        sz=$(stat -c %s $lg); o=${OFF[$1]}; [ $sz -lt $o ] && o=0; OFF[$1]=$sz
        tail -c +$((o+1)) $lg | head -c $((sz-o)) > $O/.chunk.$1
        x=$(grep -a 'mconn rescue: ' $O/.chunk.$1 | tail -1); [ -n "$x" ] && echo "$t $1 $x" >> $O/resc.txt
        n=$(grep -a -c -E 'panic|fatal error|goroutine [0-9]+ \[' $O/.chunk.$1)
        [ "$n" -gt 0 ] && { echo "$t $1 PANICLIKE $n" >> $O/alarm.txt; grep -a -m5 -E 'panic|fatal error' $O/.chunk.$1 >> $O/alarm.txt; }
        rm -f $O/.chunk.$1
      done
      sleep 30; done ) &
  bgslot(){ i=$1; port=$((8451+i)); kp=$((5211+i)); RANDOM=$((147+i*7)); sleep $((i*3))
    while [ ! -e $O/STOPS ] && [ $(el) -lt $((TT-40)) ]; do
      RS=(20 30 50 80 100 120 150 200); r=${RS[$((RANDOM % 8))]}; d=$((30 + RANDOM % 121)); dir=d; R=-R
      [ $((RANDOM % 10)) -ge 7 ] && { dir=u; R=""; }
      [ $(( $(el) + d )) -gt $((TT-30)) ] && d=$((TT - 30 - $(el))); [ $d -lt 10 ] && break
      s=$(el); timeout -k 5 $((d+25)) ip netns exec ir$ID iperf3 -c 127.0.0.1 -p $port --connect-timeout 5000 -t $d -b ${r}M $R -J > $O/bg$i.tmp 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/bg$i.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$s $(el) slot=$i dir=$dir rate=$r dur=$d mbit=$v rc=$rc" >> $O/bg.txt
      [ "$v" = ERR ] && { cp $O/bg$i.tmp $O/tests/bgerr-$i-$s.txt; ksrv $kp; }
      sleep $((5 + RANDOM % 36)); done; rm -f $O/bg$i.tmp; }
  for i in $(seq 0 $((NBG-1))); do bgslot $i & done
  ( t=60; while [ $t -lt $((TT-40)) ]; do while [ $(el) -lt $t ]; do sleep 1; done
      [ -e $O/STOPS ] && break
      ksrv 5202
      timeout -k 5 45 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 -P 1 -t 12 -R -J > $O/tests/t$t.json 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/tests/t$t.json'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$t $(el) test_mbit=$v rc=$rc" >> $O/tests.txt; t=$((t+120)); done
      for p in $(lpid 5202); do kill $p 2>/dev/null; done ) &
  ( ip netns exec ir$ID python3 $D/rt/callers.py 127.0.0.1:8443 ${NCALL:-30} $O/calls.jsonl $O/t0 & echo $! > $O/callers.pid; wait ) &
  ( while [ $(el) -lt 30 ]; do sleep 1; done; echo "$(el) idle open n=300 hold=300" >> $O/actions.txt
    ip netns exec ir$ID python3 $R/idle139.py 127.0.0.1:8443 300 300 $O/idle.jsonl ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rt169 bash $R/run147.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; kill $(cat $O/callers.pid) 2>/dev/null; sleep 6
for s in ir kh; do grep -a -E 'mconn rescue' $W/$s.log | tail -3 > $O/$s.rescue-last.txt
  grep -a -E ' (iran|exit) (rescue|RESCUE|RREC|rrec)' $W/$s.log | gzip > $O/$s.rtrace.gz
  grep -a -c -E 'panic|fatal error' $W/$s.log > $O/$s.panics; done
tail -1 $O/run.out > $O/result.json
sleep 5; kill $AGG 2>/dev/null; rm -f $FIFO
for p in $(pidof iperf3); do [ $(stat -c %Y /proc/$p) -ge $(stat -c %Y $O/env.txt) ] || continue
  ls -l /proc/$p/fd 2>/dev/null | grep -q /opt/hs2lab/lab && { echo "kill leaked iperf3 $p" >> $O/actions.txt; kill $p; }; done
mkdir -p $O/full; gzip -c $W/ir.log > $O/full/ir.log.gz; gzip -c $W/kh.log > $O/full/kh.log.gz; rm -rf $W
echo "done $(date -u +%FT%TZ)" > $O/DONE

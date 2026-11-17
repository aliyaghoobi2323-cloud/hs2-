#!/bin/bash
# rel64soak.sh (lab1, unit rel64soak): >= 4 h soak of rel64 BOTH sides (no env besides pprof + HS2_MCONN_STRIPE_LOG
# for the keepstall counters) in the PRODUCTION TOPOLOGY (run.sh TOPO146=1: 6 Iran IPv4 x 2 kharej IPv4 + IPv6 =
# 13 paths). Iran IP i = lab paths k(2i-2),k(2i-1) over KV4=(0 2 3 ... 12): IP1={0,2} IP2={3,4} IP3={5,6}
# IP4={7,8} IP5={9,10} IP6={11,12}; path 1 = the IPv6 pair.
# Crowd 6000 (life 40 s), call 2 mbit, game, NBG background heavy users (iperf3 loops), a test download every
# 120 s, idle groups (idle139.py). Events every 1800 s (c = cycle start):
#   c+90   IP1 dark (k0,k2) 60 s                                   window ip1      90-210
#   c+240  30 % RST on Iran IP2 10.60.2.1:2096 120 s                    window rst     240-360
#   c+390  queue: k5 delay 80 ms 150 s                             window queue   390-540
#   c+600  IP2+IP3 dark (k3..k6); c+602 (odd cycles c+601) staggered IP5+IP6 dark (k9..k12) = the fresh other-IP links the
#          first rescue joins go to die right after the join (RETRY trigger); all back c+660   window ip2x 600-780
#   c+810  flap IP4 (k7,k8) 25 s dark / 25 s clean to c+1050       window flap    810-1110
#   c+1290 loss_all (+0.6 %, 53 ms) 150 s                          window loss_all 1290-1440
#   c+1500 loss k9 3 % then 10 % to c+1620                         window loss_k9 1500-1620
# Per minute (proc.txt): RSS, goroutines, fds, tcp, links; resc.txt: the last "mconn rescue" line (raw | rec |
# rescue145 retry counters) per side; keep.txt: last keepstall counters line per side; alarm.txt: panic-like lines.
# Run: systemd-run --unit=rel64soak --collect /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/rel64/rel64soak.sh
set -u
R=/opt/hs2lab/rr155
O=${O:?}; L=$R/labsrc/lab
SB=${SB:-$R/hs2}; SK=${SK:-$R/hs2}; TT=${TT:-1000}; NBG=${NBG:-5}; CY=500
export P3_OUT=$O; mkdir -p $O/logs $O/tests
WD=$O/w; W=$WD$LAB_ID; ID=$LAB_ID
TR="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_MCONN_TRACE=1 HS2_MCONN_RESCUE_TRACE=1 HS2_END_LOG=1"; IRE="$TR ${XENV:-}"; KHE="$TR ${XENV:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
[ -x $R/run147.sh ] || python3 $R/mkrun147.py $L/run.sh $R/run147.sh
export KHAREJ_EXTRA="\"max_links\":260,\"min_links\":200,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1 TOPO146=1 KEEP_BACKLOG=1
J="1500us 25% distribution normal"; JU=${J// /_}
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2 0.3 0.1 0.4)
XL=(A B C D E F G H I J K L M)
for k in $(seq 0 12); do export PATH_${XL[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
base(){ echo "$1:$2:delay_45ms_${JU}_loss_random_${B[$2]}%"; }
bh(){ echo "$1:$2:delay_45ms_${JU}_loss_random_100%"; }
EV=""; c=0; : > $O/events.txt
while [ $c -lt $TT ]; do
  for k in 0 2; do EV="$EV $(bh $((c+20)) $k) $(base $((c+80)) $k)"; done
  for k in 3 4 5 6; do EV="$EV $(bh $((c+160)) $k) $(base $((c+220)) $k)"; done
  for k in 9 10 11 12; do EV="$EV $(bh $((c+162)) $k) $(base $((c+220)) $k)"; done
  t=230; on=1; while [ $t -lt 330 ]; do
    for k in 7 8; do if [ $on = 1 ]; then EV="$EV $(bh $((c+t)) $k)"; else EV="$EV $(base $((c+t)) $k)"; fi; done
    on=$((1-on)); t=$((t+25)); done
  for k in 7 8; do EV="$EV $(base $((c+330)) $k)"; done
  for k in $(seq 0 12); do l=$(python3 -c "print(round(${B[$k]}+0.6,2))"); EV="$EV $((c+340)):$k:delay_53ms_${JU}_loss_random_$l% $(base $((c+490)) $k)"; done
  printf %sn "$((c+20)) $((c+140)) ip1" "$((c+90)) $((c+150)) rst" "$((c+160)) $((c+230)) ip2x" "$((c+230)) $((c+340)) flap" "$((c+340)) $((c+490)) loss_all" >> $O/events.txt
  c=$((c+CY)); done
export NETEM_EVENTS="$EV"
echo "$EV" | tr ' ' '\n' > $O/netem-plan.txt
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp $R/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=200 MAX_LINKS=260 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=13 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 240); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  # iperf3 servers start with every fd > 2 closed: none may keep a labslot lock open after the soak
  ksrv(){ for p in $(lpid $1); do kill $p 2>/dev/null; done; sleep 0.3; for p in $(lpid $1); do kill -9 $p 2>/dev/null; done
    ( for f in $(ls /proc/$BASHPID/fd); do [ $f -gt 2 ] && eval "exec $f>&-"; done
      exec ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p $1 >/dev/null 2>&1 ) &
    local w; for w in $(seq 1 25); do ip netns exec kh$ID ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  ( declare -A OFF=([ir]=0 [kh]=0)
    while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      lk=$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(d.get('links','-'))" $SF 2>/dev/null || echo -)
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || { echo "$t $1 MISSING" >> $O/proc.txt; continue; }
        r=$(awk '/VmRSS/{print $2}' /proc/$p/status); g=$(ip netns exec $1$ID curl -s -m 2 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -1 | awk '{print $NF}')
        so=$(ip netns exec $1$ID ss -tanH 2>/dev/null | awk '{n++} $1=="ESTAB"{e++; if($4~/:2096$/||$5~/:2096$/)l++} END{printf "tcp=%d estab=%d tcp2096=%d", n, e, l}')
        echo "$t $1 pid=$p rss_kb=$r gor=$g fds=$(ls /proc/$p/fd | wc -l) $so links=$lk" >> $O/proc.txt
        lg=$W/$1.log; [ -e $lg ] || continue
        sz=$(stat -c %s $lg); o=${OFF[$1]}; [ $sz -lt $o ] && o=0; OFF[$1]=$sz
        tail -c +$((o+1)) $lg | head -c $((sz-o)) > $O/.chunk.$1
        x=$(grep -a 'mconn rescue: ' $O/.chunk.$1 | tail -1); [ -n "$x" ] && echo "$t $1 $x" >> $O/resc.txt
        x=$(grep -a 'keepstall: kept_marked' $O/.chunk.$1 | tail -1); [ -n "$x" ] && echo "$t $1 $x" >> $O/keep.txt
        n=$(grep -a -c -E 'panic|fatal error|goroutine [0-9]+ \[' $O/.chunk.$1)
        [ "$n" -gt 0 ] && { echo "$t $1 PANICLIKE $n" >> $O/alarm.txt; grep -a -m5 -E 'panic|fatal error' $O/.chunk.$1 >> $O/alarm.txt; }
        echo "$t $1 logMB=$((sz>>20))" >> $O/logsize.txt
        rm -f $O/.chunk.$1
      done
      echo "$t $(df -m /opt | tail -1 | awk '{print $4}')MBfree" >> $O/df.txt
      sleep 60; done ) &
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
      ksrv 5202; s=$(date +%s.%N)
      timeout -k 5 45 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 -P 1 -t 12 -R -J > $O/tests/t$t.json 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/tests/t$t.json'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      nbg=$(ip netns exec ir$ID ss -tnH state established '( dport >= :8451 and dport <= :8470 )' 2>/dev/null | wc -l)
      echo "$t $(el) start=$s test_mbit=$v rc=$rc bg_conns=$nbg" >> $O/tests.txt; t=$((t+120)); done
      for p in $(lpid 5202); do kill $p 2>/dev/null; done ) &
  ( c=0; RR="-p tcp -d 10.60.2.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset"
    while [ $c -lt $TT ]; do
      while [ $(el) -lt $((c+90)) ]; do sleep 1; done; [ -e $O/STOPS ] && break
      ip netns exec ir$ID iptables -I INPUT $RR; echo "$(el) rst on" >> $O/actions.txt
      while [ $(el) -lt $((c+150)) ] && [ ! -e $O/STOPS ]; do sleep 1; done
      ip netns exec ir$ID iptables -D INPUT $RR; echo "$(el) rst off" >> $O/actions.txt
      c=$((c+CY)); done ) &
  ( ip netns exec ir$ID python3 $R/callers.py 127.0.0.1:8443 ${NCALL:-30} $O/calls.jsonl $O/t0 & echo $! > $O/callers.pid; wait ) &
  ( c=0; ig(){ while [ $(el) -lt $1 ]; do sleep 1; done; [ -e $O/STOPS ] && return
        echo "$(el) idle $2 open n=300 hold=$3" >> $O/actions.txt
        ip netns exec ir$ID python3 $R/idle139.py 127.0.0.1:8443 300 $3 $O/idle-$2.jsonl & }
    while [ $c -lt $((TT-CY+1)) ]; do
      ig $((c+30)) i5c$c 300; c=$((c+CY)); done; wait ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=cd152rep bash $R/run147.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; kill $(cat $O/callers.pid) 2>/dev/null; sleep 6
for s in ir kh; do grep -a -E 'keepstall: kept_marked' $W/$s.log | tail -2000 > $O/$s.keepstall.txt
  grep -a -E 'rescue.*retry|mconn rescue' $W/$s.log | tail -3000 > $O/$s.rescue.txt
  grep -a -c -E 'panic|fatal error' $W/$s.log > $O/$s.panics
  grep -a -v -E 'stripe|keepstall|lite pick' $W/$s.log | gzip > $O/logs/$s.nostripe.log.gz; done
cp $W/netemevents.txt $O/ 2>/dev/null
tail -1 $O/run.out > $O/result.json
sleep 5; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
# leaked iperf3 (started after this soak) holding a lab lock fd: kill by exact PID
for p in $(pidof iperf3); do [ $(stat -c %Y /proc/$p) -ge $(stat -c %Y $O/env.txt) ] || continue
  ls -l /proc/$p/fd 2>/dev/null | grep -q /opt/hs2lab/lab && { echo "kill leaked iperf3 $p (holds a lab lock)" >> $O/actions.txt; kill $p; }; done
mkdir -p $O/full; mv $W/ir.log $W/kh.log $O/full/ 2>/dev/null

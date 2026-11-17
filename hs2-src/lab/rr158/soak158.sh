#!/bin/bash
# soak158.sh (lab3) = lab2 r65s/soak65s.sh (the standard long soak, read-only copy) for rr158 BOTH sides with
# HS2_MCONN_RESCUE_RST=1 (no STRIPE_STEER/UPLOSS), default env otherwise. Same load and events (1800 s cycle:
# blackholes A/G/E/B/H, flap F, loss/queue, loss_all, 30 % RST on 2096 at c+300/1110/1500, NBG iperf3 users,
# test download every 120 s, idle groups). Helpers copied to /opt/hs2lab/rr158 (run141.sh agg.py idle139.py).
# Per minute resc.txt: the last "mconn rescue-rst" counters per side.
# Run: systemd-run --unit=rr158soak --collect /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/rr158/soak158.sh
# Run: labslot.sh --all bash soak142.sh (unit rel63csoak).
set -u
O=${O:-/opt/hs2lab/rr158/soak}; L=/opt/hs2lab/lbv81/labsrc/lab
SB=${SB:-/opt/hs2lab/rr158/hs2-rr158}; SK=${SK:-/opt/hs2lab/rr158/hs2-rr158}; TT=${TT:-14400}; NBG=${NBG:-9}
export P3_OUT=$O; mkdir -p $O/logs $O/tests
WD=/tmp/rr158soakw; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_RESCUE_RST=1"; KHE="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_RESCUE_RST=1"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":200,\"min_links\":150,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2)
X=(A B C D E F G H I J)
for k in $(seq 0 9); do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
JU=${J// /_}
base(){ echo "delay_45ms_${JU}_loss_random_${B[$1]}%"; }
BH(){ echo "delay_45ms_${JU}_loss_random_100%"; }
EV=""; c=0; : > $O/events.txt
while [ $c -lt $TT ]; do
  EV="$EV $((c+90)):0:$(BH) $((c+150)):0:$(base 0)"
  EV="$EV $((c+150)):2:delay_45ms_${JU}_loss_random_3% $((c+240)):2:delay_45ms_${JU}_loss_random_10% $((c+330)):2:$(base 2)"
  EV="$EV $((c+390)):3:delay_80ms_${JU}_loss_random_${B[3]}% $((c+540)):3:$(base 3)"
  EV="$EV $((c+600)):4:delay_45ms_loss_random_100% $((c+720)):4:$(base 4)"
  t=810; on=1; while [ $t -lt 1050 ]; do
    if [ $on = 1 ]; then EV="$EV $((c+t)):5:delay_45ms_loss_random_100%"; else EV="$EV $((c+t)):5:$(base 5)"; fi
    on=$((1-on)); t=$((t+25)); done
  EV="$EV $((c+1050)):5:$(base 5)"
  EV="$EV $((c+990)):1:$(BH) $((c+1050)):1:$(base 1)"
  EV="$EV $((c+450)):6:$(BH) $((c+510)):6:$(base 6) $((c+1650)):7:$(BH) $((c+1710)):7:$(base 7)"
  for k in $(seq 0 9); do l=$(python3 -c "print(${B[$k]}+0.6)"); EV="$EV $((c+1290)):$k:delay_53ms_${JU}_loss_random_$l% $((c+1440)):$k:$(base $k)"; done
  printf '%s\n' "$((c+90)) $((c+210)) bhA" "$((c+240)) $((c+390)) loss_c" "$((c+390)) $((c+540)) queue_d" \
    "$((c+600)) $((c+780)) bh_e120" "$((c+810)) $((c+990)) flap_f" "$((c+990)) $((c+1110)) bhB" \
    "$((c+1110)) $((c+1230)) rst_g" "$((c+1290)) $((c+1440)) loss_all"     "$((c+300)) $((c+420)) rst_a" "$((c+450)) $((c+570)) bhG" "$((c+1500)) $((c+1620)) rst_b" "$((c+1650)) $((c+1770)) bhH" >> $O/events.txt
  c=$((c+1800)); done
export NETEM_EVENTS="$EV"
echo "$EV" | tr ' ' '\n' > $O/netem-plan.txt
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp /opt/hs2lab/rr158/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  ksrv(){ for p in $(lpid $1); do kill $p 2>/dev/null; done; sleep 0.3; for p in $(lpid $1); do kill -9 $p 2>/dev/null; done
    ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p $1 >/dev/null 2>&1 &
    for i in $(seq 1 25); do ip netns exec kh$ID ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  ( declare -A OFF=([ir]=0 [kh]=0); h=0
    while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || { echo "$t $1 MISSING" >> $O/proc.txt; continue; }
        r=$(awk '/VmRSS/{print $2}' /proc/$p/status); g=$(ip netns exec $1$ID curl -s -m 2 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -1 | awk '{print $NF}')
        so=$(ip netns exec $1$ID ss -tanH 2>/dev/null | awk '{n++} $1=="ESTAB"{e++; if($4~/:2096$/||$5~/:2096$/)l++} END{printf "tcp=%d estab=%d tcp2096=%d", n, e, l}')
        echo "$t $1 pid=$p rss_kb=$r gor=$g fds=$(ls /proc/$p/fd | wc -l) $so" >> $O/proc.txt
        lg=$W/$1.log; [ -e $lg ] || continue
        n=$(tail -c 400000 $lg | grep -a -c -E 'panic|fatal error|goroutine [0-9]+ \[')
        [ "$n" -gt 0 ] && echo "$t $1 PANICLIKE $n" >> $O/alarm.txt
        echo "$t $1 $(tail -c 400000 $lg | grep -a -o "mconn rescue-rst:.*" | tail -1)" >> $O/resc.txt
      done
      sleep 60; done ) &
  ( while [ ! -e $O/STOPS ]; do t=$(el); [ $t -gt $((TT+60)) ] && break
      python3 - "$SF" "$t" >> $O/stripe.txt 2>/dev/null <<'PY'
import json, sys
try: d = json.load(open(sys.argv[1]))
except Exception: d = {}
print(sys.argv[2], d.get("stripe_active", 0), d.get("stripe_refused", 0), d.get("stripe_subs", 0), d.get("links", "-"))
PY
      sleep 10; done ) &
  bgslot(){ i=$1; port=$((8451+i)); kp=$((5211+i)); RANDOM=$((142+i*7)); sleep $((i*3))
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
  ( c=0; RR="-p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset"
    while [ $c -lt $TT ]; do
      for w in "300 420" "1110 1230" "1500 1620"; do set -- $w
        [ $1 -ge $TT ] && break 2
        while [ $(el) -lt $((c+$1)) ]; do sleep 1; done; [ -e $O/STOPS ] && break 2
        ip netns exec ir$ID iptables -I INPUT $RR; echo "$(el) rst on" >> $O/actions.txt
        while [ $(el) -lt $((c+$2)) ] && [ ! -e $O/STOPS ]; do sleep 1; done
        ip netns exec ir$ID iptables -D INPUT $RR; echo "$(el) rst off" >> $O/actions.txt; done
      c=$((c+1800)); done ) &
  ( c=0; ig(){ while [ $(el) -lt $1 ]; do sleep 1; done; [ -e $O/STOPS ] && return
        echo "$(el) idle $2 open n=300 hold=$3" >> $O/actions.txt
        ip netns exec ir$ID python3 /opt/hs2lab/rr158/idle139.py 127.0.0.1:8443 300 $3 $O/idle-$2.jsonl & }
    while [ $c -lt $((TT-1800+1)) ]; do
      ig $((c+30)) i25c$c 1500; ig $((c+200)) i10c$c 600; ig $((c+900)) i5c$c 300; ig $((c+1000)) i12c$c 720
      c=$((c+1800)); done; wait ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rr158soak bash /opt/hs2lab/rr158/run141.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
cp $W/netemevents.txt $O/ 2>/dev/null
tail -1 $O/run.out > $O/result.json
zcat $O/logs/ir.full.log.gz 2>/dev/null | grep -a -E "download stripe at|striped over|dlfair" > $O/stripe.log
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -E "stripe steer|lb auto2 upload" > $O/$s.steer.txt; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -E "asked-dial|steer-held|asked-dial-dropped" | tail -3000 > $O/$s.askdial.txt; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -E 'keepstall|ack-stall|hole room' | tail -2000 > $O/$s.keepstall.txt; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -c -E 'panic|fatal error' > $O/$s.panics; done
sleep 5; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
# leaked iperf3 (started after this soak) holding a labslot lock fd: kill by exact PID
for p in $(pidof iperf3); do [ $(stat -c %Y /proc/$p) -ge $(stat -c %Y $O/env.txt) ] || continue
  ls -l /proc/$p/fd 2>/dev/null | grep -q /opt/hs2lab/lab && { echo "kill leaked iperf3 $p (holds a lab lock)" >> $O/actions.txt; kill $p; }; done
rm -rf $W

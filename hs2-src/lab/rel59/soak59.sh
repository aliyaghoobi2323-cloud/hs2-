#!/bin/bash
# soak59.sh: ~6 h soak of rel59 on both sides (edge and exit = $SB), prod-like lab
# (reverse l3mtcp, 10 paths ~90 ms with jitter, base loss 0.1-0.8 %, crowd 3000 with
# reconnect churn (life 40 s), call 2 mbit, game, ping), heavy every 300 s (40 s
# download -P4 then 25 s upload -P2), and a 30 min path-event cycle repeated all run
# long: loss 3 %/10 % on c, +35 ms queue on d, blackhole e, flap f (25 s), RST storm
# on g (30 %), mild loss on all. No restarts (so RSS/goroutines/fds trend = leak check).
# Every 60 s: proc.txt (t side pid rss_kb gor fds) and st.jsonl (links/users/lb).
# Run under labslot.sh --all. Output: $O (default /opt/hs2lab/rel59soak).
set -u
O=${O:-/opt/hs2lab/rel59soak}; L=/opt/hs2lab/lbv81/labsrc/lab
SB=${SB:-/opt/hs2lab/rel59/hs2R59}; TT=${TT:-21600}
export P3_OUT=$O; mkdir -p $O
WD=/tmp/rel59soak; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060"; KHE="HS2_PPROF=127.0.0.1:6060"
echo "edge=exit=$(sha256sum $SB|cut -c1-8) TT=$TT start=$(date -u +%FT%TZ)" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":200,\"min_links\":150,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2)
X=(A B C D E F G H I J)
for k in $(seq 0 9); do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
JU=${J// /_}
base(){ echo "delay_45ms_${JU}_loss_random_${B[$1]}%"; }
EV=""; c=0
while [ $c -lt $TT ]; do
  EV="$EV $((c+150)):2:delay_45ms_${JU}_loss_random_3% $((c+240)):2:delay_45ms_${JU}_loss_random_10% $((c+330)):2:$(base 2)"
  EV="$EV $((c+390)):3:delay_80ms_${JU}_loss_random_${B[3]}% $((c+540)):3:$(base 3)"
  EV="$EV $((c+600)):4:delay_45ms_loss_random_100% $((c+720)):4:$(base 4)"
  t=810; on=1; while [ $t -lt 1050 ]; do
    if [ $on = 1 ]; then EV="$EV $((c+t)):5:delay_45ms_loss_random_100%"; else EV="$EV $((c+t)):5:$(base 5)"; fi
    on=$((1-on)); t=$((t+25)); done
  EV="$EV $((c+1050)):5:$(base 5)"
  for k in $(seq 0 9); do l=$(python3 -c "print(${B[$k]}+0.6)"); EV="$EV $((c+1290)):$k:delay_53ms_${JU}_loss_random_$l% $((c+1440)):$k:$(base $k)"; done
  c=$((c+1800)); done
export NETEM_EVENTS="$EV"
echo "$EV" | tr ' ' '\n' > $O/netem-plan.txt
export T=${TT}s HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s"
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  ( while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      python3 - "$SF" "$SFK" "$t" >> $O/st.jsonl 2>/dev/null <<'PY'
import json, sys
def rd(f):
    try: return json.load(open(f))
    except Exception: return {}
ir, kh = rd(sys.argv[1]), rd(sys.argv[2])
pick = lambda d: {k: d.get(k) for k in ("links", "users", "lb_paths", "lb_text") if k in d}
print(json.dumps({"t": int(sys.argv[3]), "ir": pick(ir), "kh": pick(kh)}))
PY
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || { echo "$t $1 MISSING" >> $O/proc.txt; continue; }
        r=$(awk '/VmRSS/{print $2}' /proc/$p/status); g=$(ip netns exec $1$ID curl -s -m 2 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -1 | awk '{print $NF}')
        ln=$(ip netns exec $1$ID ss -tnH state established 2>/dev/null | grep -c ':2096')
        echo "$t $1 pid=$p rss_kb=$r gor=$g fds=$(ls /proc/$p/fd | wc -l) tcp2096=$ln" >> $O/proc.txt; done
      sleep 60; done ) &
  ( t=60; while [ $t -lt $((TT-120)) ]; do while [ $(el) -lt $t ]; do sleep 1; done
      ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 40 -R -J > $O/hv.tmp 2>&1
      d=$(python3 -c "import json;print(round(json.load(open('$O/hv.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 2 -t 25 -J > $O/hv.tmp 2>&1
      u=$(python3 -c "import json;print(round(json.load(open('$O/hv.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$t $(el) down=$d up=$u" >> $O/heavy.txt; t=$((t+300)); done; rm -f $O/hv.tmp ) &
  ( c=0; while [ $c -lt $TT ]; do
      while [ $(el) -lt $((c+1110)) ]; do sleep 1; done; [ -e $O/STOPS ] && break
      ip netns exec ir$ID iptables -I INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
      echo "$(el) rst on" >> $O/actions.txt
      while [ $(el) -lt $((c+1230)) ]; do sleep 1; done
      ip netns exec ir$ID iptables -D INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
      echo "$(el) rst off" >> $O/actions.txt; c=$((c+1800)); done ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SB IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 LOGDIR=$O RUNTAG=rel59soak bash $L/run.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
cp $W/ir.log $W/kh.log $O/ 2>/dev/null; gzip -f $O/ir.log $O/kh.log 2>/dev/null
tail -1 $O/run.out > $O/result.json
echo "done $(date -u +%FT%TZ)" > $O/DONE
rm -rf $W

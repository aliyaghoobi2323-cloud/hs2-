#!/bin/bash
# lbv81.sh TAG: one ~31 min scripted LB soak with the production binaries
# (edge $D/hs2E = bin79 dec281e0, exit $D/hs2X = rel58 5d87000d, no env keys:
# the code defaults are the production set; HS2_LB_FAST=0 so the judge runs at
# production speed: quality tick 30 s, rebalance 60 s).
# 10 paths (path b IPv6), rtt ~90 ms, base loss 0.1-0.8 %, reverse l3mtcp,
# crowd 3000 with reconnect churn (life 20-60 s), call 2 mbit, game, hs0 ping,
# periodic heavy down x4 / up x2 through the tunnel, and the event timeline in
# events.txt (t = seconds from the probe start). Run under labslot.sh --all.
set -u
TAG=$1; D=/opt/hs2lab/lbv81; L=$D/labsrc/lab
EB=${HS2E:-$D/hs2E}; XB=${HS2X:-$D/hs2X}   # binaries (default: the production pair)
export P3_OUT=$D/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/lbv81-$TAG; W=$WD$LAB_ID; ID=$LAB_ID
# ARM="HS2_X=1 ..." adds env keys to both sides; SHORT=1: 8 min, no events (A/B of heavy-start behaviour)
IRE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${ARM:-}"
echo "IRE=$IRE KHE=$KHE edge=$(sha256sum $EB|cut -c1-8) exit=$(sha256sum $XB|cut -c1-8)" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":200,\"min_links\":150,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2)   # base loss per path a..j
X=(A B C D E F G H I J)
for k in $(seq 0 9); do export PATH_${X[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
JU=${J// /_}
base(){ echo "delay_45ms_${JU}_loss_random_${B[$1]}%"; }
# timeline (paths: 2=c loss, 3=d queue, 4=e blackhole, 5=f flap, 6=g RST, all=mild)
EV=""
EV="$EV 150:2:delay_45ms_${JU}_loss_random_3% 240:2:delay_45ms_${JU}_loss_random_10% 330:2:$(base 2)"
EV="$EV 390:3:delay_80ms_${JU}_loss_random_${B[3]}% 540:3:$(base 3)"
EV="$EV 600:4:delay_45ms_loss_random_100% 720:4:$(base 4)"
t=810; on=1; while [ $t -lt 1050 ]; do
  if [ $on = 1 ]; then EV="$EV $t:5:delay_45ms_loss_random_100%"; else EV="$EV $t:5:$(base 5)"; fi
  on=$((1-on)); t=$((t+25)); done
EV="$EV 1050:5:$(base 5)"
for k in $(seq 0 9); do l=$(python3 -c "print(${B[$k]}+0.6)"); EV="$EV 1290:$k:delay_53ms_${JU}_loss_random_$l% 1440:$k:$(base $k)"; done
[ -n "${SHORT:-}" ] && EV=""
export NETEM_EVENTS="$EV"
echo "$EV" | tr ' ' '\n' > $P3_OUT/netem-plan.txt
cat > $P3_OUT/events.txt <<T
0 start
150 a_loss3 c
240 a_loss10 c
330 a_clear c
390 b_queue d +35ms
540 b_clear d
600 c_black e
720 c_clear e
810 d_flap_start f
1050 d_flap_end f
1110 e_rst_start g
1230 e_rst_end g
1290 f_mild_all
1440 f_clear_all
1500 g_restart_edge
1620 g_restart_exit
1680 h_clean
1860 end
T
TT=1860s; [ -n "${SHORT:-}" ] && TT=500s
[ -n "${SHORT:-}" ] && export HVEND=440
export T=$TT HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=${CROWD_N:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0 LB_SAMPLE=1
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $P3_OUT/samples.txt"
# side jobs (all relative to the probe start = when 8443 answers)
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $P3_OUT/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  # sampler: every 5 s status of both sides, ss per kharej link socket, rss, goroutines
  ( while [ ! -e $P3_OUT/STOPS ]; do
      t=$(el); [ $t -gt 1880 ] && break
      echo "@T $t" >> $P3_OUT/ss.txt
      ip netns exec kh$ID ss -tinH state established '( dport = :2096 )' >> $P3_OUT/ss.txt 2>/dev/null
      python3 - "$SF" "$SFK" "$t" >> $P3_OUT/st.jsonl 2>/dev/null <<'PY'
import json, sys
def rd(f):
    try: return json.load(open(f))
    except Exception: return {}
ir, kh = rd(sys.argv[1]), rd(sys.argv[2])
pick = lambda d: {k: d.get(k) for k in ("links", "users", "lb_paths", "lb_pairs", "lb_verdicts", "lb", "lb_text") if k in d}
print(json.dumps({"t": int(sys.argv[3]), "ir": pick(ir), "kh": pick(kh), "irx": {k: v for k, v in ir.items() if k.startswith(("stripe_gate", "aw_floor", "rescue", "call", "cut", "drain"))}, "khx": {k: v for k, v in kh.items() if k.startswith(("stripe_gate", "aw_floor", "rescue", "call", "cut", "drain", "flap"))}}))
PY
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || continue
        r=$(awk '/VmRSS/{print $2}' /proc/$p/status); g=$(ip netns exec $1$ID curl -s -m 2 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -1 | awk '{print $NF}')
        echo "$t $1 pid=$p rss_kb=$r gor=$g fds=$(ls /proc/$p/fd | wc -l)" >> $P3_OUT/proc.txt; done
      sleep 5; done ) &
  # heavy users: every 120 s from t=60: 40 s download x4, then 25 s upload x2 (iperf3 via 8444)
  ( t=60; while [ $t -lt ${HVEND:-1800} ]; do while [ $(el) -lt $t ]; do sleep 1; done
      ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 40 -R -J > $P3_OUT/hv-d-$t.json 2>&1
      ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 2 -t 25 -J > $P3_OUT/hv-u-$t.json 2>&1
      echo "$t $(el)" >> $P3_OUT/heavy-times.txt; t=$((t+120)); done ) &
  # RST storm on path g (Iran IP 192.168.56.1): 30 % of incoming segments answered with RST
  [ -z "${SHORT:-}" ] && ( while [ $(el) -lt 1110 ]; do sleep 1; done
    ip netns exec ir$ID iptables -I INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
    echo "$(el) rst on" >> $P3_OUT/actions.txt
    while [ $(el) -lt 1230 ]; do sleep 1; done
    ip netns exec ir$ID iptables -D INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
    echo "$(el) rst off $(ip netns exec ir$ID iptables -L INPUT -n | wc -l)" >> $P3_OUT/actions.txt ) &
  # restarts: edge (Iran) at 1500, exit (kharej) at 1620
  [ -z "${SHORT:-}" ] && ( while [ $(el) -lt 1500 ]; do sleep 1; done
    p=$(hpid ir hs2 | head -1); kill $p; sleep 1; kill -9 $p 2>/dev/null; echo "$(el) edge killed $p" >> $P3_OUT/actions.txt
    ip netns exec ir$ID env HS2_NO_TUNE=1 $IRE $W/hs2 run -c $W/ir.json >> $W/ir.log 2>&1 & echo $! >> $W/pids
    echo "$(el) edge started $!" >> $P3_OUT/actions.txt
    while [ $(el) -lt 1620 ]; do sleep 1; done
    p=$(hpid kh hs2kh | head -1); kill $p; sleep 1; kill -9 $p 2>/dev/null; echo "$(el) exit killed $p" >> $P3_OUT/actions.txt
    ip netns exec kh$ID env HS2_NO_TUNE=1 $KHE $W/hs2kh run -c $W/kh.json >> $W/kh.log 2>&1 & echo $! >> $W/pids
    echo "$(el) exit started $!" >> $P3_OUT/actions.txt ) &
  wait ) > /dev/null 2>&1 &
SJ=$!
env BIN=$EB BIN_KH=$XB IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
touch $P3_OUT/STOPS; sleep 6
cp $W/ir.log $W/kh.log $W/netemevents.txt $W/lbsamp.jsonl $W/ping.txt $W/lbtime.txt $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} done $(date)" > $P3_OUT/meta.txt

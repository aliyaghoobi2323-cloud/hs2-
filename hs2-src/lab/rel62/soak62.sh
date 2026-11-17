#!/bin/bash
# soak62.sh (copy of rel60 soak60.sh): >=4 h soak of rel62 on both sides (edge and exit = $SB, defaults only: SLOW_SKIP_COPY + CUT_NONEW
# edge ON, LB_SILENT exit ON, MCONN_RESCUE ON both). soak99 design (rel59 soak + a single-path 60 s
# blackhole every 15 min) with the heavy-user harness fixed after the rel59 soak: there the
# kharej iperf3 server (127.0.0.1:5202, started once by run.sh) segfaulted (iperf3 3.16) and every
# later heavy step read ERR. Here each heavy step kills any iperf3 server in kh$ID (exact PIDs),
# starts a fresh server on 5202 per client (ready = listening, checked with ss), wraps the client in timeout (down 90 s,
# up 70 s) with --connect-timeout, and on a timeout/ERR saves ss -tni of the 8444 conns (ir side)
# and the 5202 conns (kh side) to $O/hang/ before cleanup, so a hang can be told apart
# (iperf3 deadlock = conns idle with nothing queued; tunnel stall = bytes queued, not acked).
# heavy.txt: "t el down=.. up=.. rcD=<client exit> rcU=.."; silent cuts per minute in resc.txt.
# Run: labslot.sh --all bash soak60.sh (unit rel62soak).
set -u
O=${O:-/opt/hs2lab/rel62soak}; L=/opt/hs2lab/lbv81/labsrc/lab
SB=${SB:-/opt/hs2lab/rel62/hs2R62}; TT=${TT:-15000}
export P3_OUT=$O; mkdir -p $O/logs
WD=/tmp/rel62soakw; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060"; KHE="HS2_PPROF=127.0.0.1:6060"
echo "edge=exit=$(sha256sum $SB|cut -c1-8) TT=$TT start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
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
  EV="$EV $((c+90)):0:$(BH) $((c+150)):0:$(base 0)"                       # soak99 blackhole A
  EV="$EV $((c+150)):2:delay_45ms_${JU}_loss_random_3% $((c+240)):2:delay_45ms_${JU}_loss_random_10% $((c+330)):2:$(base 2)"
  EV="$EV $((c+390)):3:delay_80ms_${JU}_loss_random_${B[3]}% $((c+540)):3:$(base 3)"
  EV="$EV $((c+600)):4:delay_45ms_loss_random_100% $((c+720)):4:$(base 4)"
  t=810; on=1; while [ $t -lt 1050 ]; do
    if [ $on = 1 ]; then EV="$EV $((c+t)):5:delay_45ms_loss_random_100%"; else EV="$EV $((c+t)):5:$(base 5)"; fi
    on=$((1-on)); t=$((t+25)); done
  EV="$EV $((c+1050)):5:$(base 5)"
  EV="$EV $((c+990)):1:$(BH) $((c+1050)):1:$(base 1)"                      # soak99 blackhole B
  for k in $(seq 0 9); do l=$(python3 -c "print(${B[$k]}+0.6)"); EV="$EV $((c+1290)):$k:delay_53ms_${JU}_loss_random_$l% $((c+1440)):$k:$(base $k)"; done
  # named windows for soakview99 (start end name); blackhole windows run 60 s past the end (late fails)
  printf '%s\n' "$((c+90)) $((c+210)) bhA" "$((c+240)) $((c+390)) loss_c" "$((c+390)) $((c+540)) queue_d" \
    "$((c+600)) $((c+780)) bh_e120" "$((c+810)) $((c+990)) flap_f" "$((c+990)) $((c+1110)) bhB" \
    "$((c+1110)) $((c+1230)) rst_g" "$((c+1290)) $((c+1440)) loss_all" >> $O/events.txt
  c=$((c+1800)); done
export NETEM_EVENTS="$EV"
echo "$EV" | tr ' ' '\n' > $O/netem-plan.txt
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
# per-minute aggregator: "min crowd_n crowd_p50 crowd_p99 fails call_p99 game_p99"; fails.txt: each fail sec
cat > $O/agg.py <<'PY'
import sys
fi, mo, fo = sys.argv[1:4]
M = {}; top = 0.0
mout = open(mo, "a", buffering=1); fout = open(fo, "a", buffering=1)
def q(v, p):
    if not v: return "-"
    v.sort(); return "%.1f" % v[min(len(v) - 1, int(len(v) * p))]
def emit(m):
    d = M.pop(m); c = d.get("c", [])
    mout.write("%d %d %s %s %d %s %s\n" % (m, len(c), q(c, .5), q(c, .99), d.get("F", 0), q(d.get("k", []), .99), q(d.get("g", []), .99)))
for ln in open(fi):
    p = ln.split()
    if len(p) < 3: continue
    try: s = float(p[1]); v = float(p[2])
    except ValueError: continue
    m = int(s // 60); d = M.setdefault(m, {})
    if p[0] == "F":
        d["F"] = d.get("F", 0) + 1; fout.write("%.1f\n" % s)
    else: d.setdefault(p[0], []).append(v)
    if s > top + 1:
        top = s
        for k in sorted(M):
            if k < int((top - 15) // 60): emit(k)
for k in sorted(M): emit(k)
PY
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
export T=${TT}s HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  ( declare -A OFF=([ir]=0 [kh]=0); h=0
    while [ ! -e $O/STOPS ]; do
      t=$(el); [ $t -gt $((TT+60)) ] && break
      python3 - "$SF" "$SFK" "$t" >> $O/st.jsonl 2>/dev/null <<'PY'
import json, sys
def rd(f):
    try: return json.load(open(f))
    except Exception: return {}
ir, kh = rd(sys.argv[1]), rd(sys.argv[2])
pick = lambda d: {k: d.get(k) for k in ("links", "users", "retiring", "lb_text") if k in d}
print(json.dumps({"t": int(sys.argv[3]), "ir": pick(ir), "kh": pick(kh)}))
PY
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || { echo "$t $1 MISSING" >> $O/proc.txt; continue; }
        r=$(awk '/VmRSS/{print $2}' /proc/$p/status); g=$(ip netns exec $1$ID curl -s -m 2 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | head -1 | awk '{print $NF}')
        so=$(ip netns exec $1$ID ss -tanH 2>/dev/null | awk '{n++} $1=="ESTAB"{e++; if($4~/:2096$/||$5~/:2096$/)l++} END{printf "tcp=%d estab=%d tcp2096=%d", n, e, l}')
        u=$(ip netns exec $1$ID ss -uanH 2>/dev/null | wc -l)
        echo "$t $1 pid=$p rss_kb=$r gor=$g fds=$(ls /proc/$p/fd | wc -l) $so udp=$u" >> $O/proc.txt
        lg=$W/$1.log; [ -e $lg ] || continue
        a=$(tail -c 400000 $lg | grep -a 'mconn rescue:' | tail -1 | sed 's/^.*mconn rescue:/mconn rescue:/')
        sc=$(tail -c 400000 $lg | grep -a -c -E 'silent: nothing acked|silent cut|lb silent')
        echo "$t $1 silent_lines_tail=$sc ${a:--}" >> $O/resc.txt
        n=$(tail -c 400000 $lg | grep -a -c -E 'panic|fatal error|goroutine [0-9]+ \[')
        [ "$n" -gt 0 ] && echo "$t $1 PANICLIKE $n" >> $O/alarm.txt
      done
      df -P / | awk -v t=$t 'NR==2{print t, $5, $4}' >> $O/df.txt
      if [ $((t / 3600)) -gt $h ]; then h=$((t / 3600))     # hourly: cut the new part of each hs2 log
        for s in ir kh; do lg=$W/$s.log; [ -e $lg ] || continue; sz=$(stat -c %s $lg)
          tail -c +$((OFF[$s] + 1)) $lg | head -c $((sz - OFF[$s])) | gzip > $O/logs/$s.h$(printf %02d $h).gz; OFF[$s]=$sz; done
        cp $W/ping.txt $O/logs/ping.txt 2>/dev/null && gzip -f $O/logs/ping.txt; fi
      sleep 60; done ) &
  hsrv(){ for p in $(hpid kh iperf3); do kill $p 2>/dev/null; done; sleep 0.3
    for p in $(hpid kh iperf3); do kill -9 $p 2>/dev/null; done
    ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p 5202 >/dev/null 2>&1 &
    # ready = listening (never connect to test it: iperf3 takes any connection as a client)
    for i in $(seq 1 25); do ip netns exec kh$ID ss -tlnH 'sport = :5202' 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  hdump(){ mkdir -p $O/hang; f=$O/hang/$1.txt
    { echo "== ir ss 8444"; ip netns exec ir$ID ss -tniH 2>/dev/null | grep -A1 ':8444' | head -40
      echo "== kh ss 5202"; ip netns exec kh$ID ss -tniH 2>/dev/null | grep -A1 ':5202' | head -40
      echo "== hv"; tail -c 1500 $O/hv.tmp; } > $f 2>&1; }
  heavy1(){ # $1 tag $2 timeout $3.. iperf3 client args; prints "mbit rc"
    hsrv; timeout -k 5 $2 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 "${@:3}" -J > $O/hv.tmp 2>&1; rc=$?
    v=$(python3 -c "import json;print(round(json.load(open('$O/hv.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
    [ "$v" = ERR ] && hdump $1
    echo "$v $rc"; }
  ( t=60; while [ $t -lt $((TT-120)) ]; do while [ $(el) -lt $t ]; do sleep 1; done
      [ -e $O/STOPS ] && break
      set -- $(heavy1 d$t 90 -P 4 -t 40 -R); d=$1; rd=$2
      set -- $(heavy1 u$t 70 -P 2 -t 25); u=$1; ru=$2
      echo "$t $(el) down=$d up=$u rcD=$rd rcU=$ru" >> $O/heavy.txt; t=$((t+300)); done; rm -f $O/hv.tmp
      for p in $(hpid kh iperf3); do kill $p 2>/dev/null; done ) &
  ( c=0; while [ $c -lt $TT ]; do
      while [ $(el) -lt $((c+1110)) ]; do sleep 1; done; [ -e $O/STOPS ] && break
      ip netns exec ir$ID iptables -I INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
      echo "$(el) rst on" >> $O/actions.txt
      while [ $(el) -lt $((c+1230)) ]; do sleep 1; done
      ip netns exec ir$ID iptables -D INPUT -p tcp -d 192.168.56.1 --dport 2096 -m statistic --mode random --probability 0.3 -j REJECT --reject-with tcp-reset
      echo "$(el) rst off" >> $O/actions.txt; c=$((c+1800)); done ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SB IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rel62soak bash $L/run.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
cp $W/netemevents.txt $O/ 2>/dev/null; cp $W/ping.txt $O/logs/ 2>/dev/null && gzip -f $O/logs/ping.txt
tail -1 $O/run.out > $O/result.json
sleep 5; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
rm -rf $W

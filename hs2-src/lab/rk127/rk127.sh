#!/bin/bash
# rk127.sh OFF|ON|PR TAG (rel62k, HS2_RETIRE_KEEP_IDLE confirmation; run under labslot.sh).
# OFF/ON: T=3900 (65 min), crowd 300 churn, heavy every 600 s, 12k storms (open 10 s, held 300 s,
# then closed -> pool grows then shrinks) at 200/1200/2100/2850. Idle groups (300 conns, one
# request at open, then idle, then one request): g55 open 30 -> 3330, g30 1500 -> 3310,
# g15 2400 -> 3300, g5 2990 -> 3290, g10 1000 -> 1600, g5b 600 -> 900.
# PR: key on, max_links 110 (min 100), T=1800, 12k storms every 300 s from 150 (held 150 s), so
# regrow meets kept retiring links (up > max): g25 open 30 -> 1530, g20 300 -> 1500, g5 1200 -> 1500.
set -u
SC=$1; TAG=$2; ID=$LAB_ID
R=/opt/hs2lab/rel62k; R4=/opt/hs2lab/ec124; R5=/opt/hs2lab/ec125; O=$R/res/$TAG; L=/opt/hs2lab/lbv81/labsrc/lab; SB=$R/hs2K
KX=""; MX=150; TT=3900
[ $SC = ON ] && KX="HS2_RETIRE_KEEP_IDLE=1"
[ $SC = PR ] && { KX="HS2_RETIRE_KEEP_IDLE=1"; MX=110; TT=1800; }
TT=${TTO:-$TT}
rm -rf $O; mkdir -p $O; WD=/tmp/rk127-$TAG-; W=$WD$ID
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1 KHAREJ_EXTRA="\"max_links\":$MX,\"min_links\":100,"
X=(A B C D E F); BL=(0.1 0.3 0.2 0.5 0.1 0.3)
for k in 0 1 2 3 4 5; do export PATH_${X[$k]}_NETEM="loss random ${BL[$k]}%"; done
export T=${TT}s HEAVY_SECS=10 HEAVY_OMIT=0 BULK=0 CALL="" GAME=0 MIN_LINKS=100 MAX_LINKS=$MX CROWD=300 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=6 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-samplelog $O/samples.txt"
echo "SC=$SC TAG=$TAG LAB_ID=$ID bin=$(sha256sum $SB|cut -c1-8) KX=$KX max=$MX T=$TT start=$(date -u +%FT%TZ)" > $O/env.txt
( SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
  for i in $(seq 1 240); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  at(){ while [ $(el) -lt $1 ]; do sleep 0.3; done; }
  ev(){ echo "$(el) $*" >> $O/events.txt; }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  ( while [ ! -e $O/STOP ]; do
      ip netns exec ir$ID ss -tniH state established '( sport = :2096 )' 2>/dev/null | python3 $R4/gt.py $(el) "$SF" >> $O/lbgt.jsonl 2>>$O/gt.err
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1)
        g=$(ip netns exec $1$ID timeout 5 curl -s "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" 2>/dev/null | head -1 | awk '{print $NF}')
        [ -n "$p" ] && echo "$(el) $1 pid=$p rss=$(awk '/VmRSS/{print $2}' /proc/$p/status) fds=$(ls /proc/$p/fd|wc -l) gr=${g:-?}" >> $O/proc.txt || echo "$(el) $1 MISSING" >> $O/proc.txt; done
      sleep 30; done ) &
  hsrv(){ for p in $(hpid kh iperf3); do kill $p 2>/dev/null; done; sleep 0.3
    for p in $(hpid kh iperf3); do kill -9 $p 2>/dev/null; done
    ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p 5202 >/dev/null 2>&1 &
    for i in $(seq 1 25); do ip netns exec kh$ID ss -tlnH 'sport = :5202' 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  heavy(){ hsrv; s=$(el); timeout -k 5 50 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 -P 4 -t 30 -R -i 1 -J > $O/hv.tmp 2>&1; rc=$?
    python3 -c '
import json, sys
f, s, rc = sys.argv[1:4]
try:
    j = json.load(open(f)); iv = [round(i["sum"]["bits_per_second"] / 1e6) for i in j["intervals"]]
    print(s, "rc=%s" % rc, "mbps=%d" % round(j["end"]["sum_received"]["bits_per_second"] / 1e6), "stall_s=%d" % sum(1 for v in iv if v < 5), "min=%d" % min(iv))
except Exception as e:
    print(s, "rc=%s" % rc, "ERR", type(e).__name__)' $O/hv.tmp $s $rc >> $O/heavy.txt; rm -f $O/hv.tmp; }
  sg(){ ev "$1 open n=$2 hold=$3"; ip netns exec ir$ID bash -c "ulimit -n 200000; $R4/stormgo 127.0.0.1:8443 $2 $4 $3 $O/storm-$1.jsonl" & }
  ( for t in $(seq 100 600 $((TT-200))); do at $t; [ -e $O/STOP ] && break; heavy; done ) &
  if [ $SC = PR ]; then
    at 30; sg g25 300 1500 5
    for t in 150 450 750 1050 1350; do at $t; sg st$t 12000 150 10; [ $t = 150 ] && { at 300; sg g20 300 1200 5; }; done
    at 1200; sg g5 300 300 5
  else
    at 30; sg g55 300 3300 5
    at 200; sg st200 12000 300 10
    at 600; sg g5b 300 300 5
    at 1000; sg g10 300 600 5
    at 1200; sg st1200 12000 300 10
    at 1500; sg g30 300 1810 5
    at 2100; sg st2100 12000 300 10
    at 2400; sg g15 300 900 5
    at 2850; sg st2850 12000 300 10
    at 2990; sg g5 300 300 5
  fi
  wait ) > $O/ctl.out 2>&1 &
env BIN=$SB BIN_KH=$SB HEAVY=down HEAVY_P=1 IR_ENV="HS2_PPROF=127.0.0.1:6060 $KX" KH_ENV="HS2_PPROF=127.0.0.1:6060 $KX" RUNTAG=rk127$TAG timeout $((TT+300)) bash $L/run.sh > $O/run.out 2> $O/run.err
touch $O/STOP; sleep 3
tail -1 $O/run.out > $O/result.json
for s in ir kh; do grep -a -c -E 'panic|fatal error' $W/$s.log > $O/$s.panics 2>/dev/null
  grep -a -E 'closed [0-9]+ connection|retire keep|retiring|retired|up: [0-9]+ serving|panic|fatal' $W/$s.log 2>/dev/null | gzip > $O/$s.sel.gz; done
python3 $R4/win.py $O > $O/windows.txt 2>&1; rm -f $O/samples.txt
rm -rf $W; echo done > $O/DONE

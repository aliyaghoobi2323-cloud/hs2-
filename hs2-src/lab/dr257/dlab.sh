#!/bin/bash
# dr253: single-stream DOWNLOAD A/B (rel69g vs rel69i). Derived from r68ft/h/soak.sh: no netem events, no RST windows,
# no idle groups; NBG light bg iperf (20-80M), single-stream 12 s iperf3 -R test every TI s from t=40.
set -u
H=/opt/hs2lab/r68ft/h; O=${O:?}
SB=${SB:?}; SK=${SK:-$SB}; TT=${TT:-260}; NBG=${NBG:-3}
export P3_OUT=$O; mkdir -p $O/logs $O/tests
WD=/tmp/dr257w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 ${XENV:-} ${IRX:-}"; KHE="HS2_PPROF=127.0.0.1:6060 ${XENV:-} ${KHX:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID" > $O/env.txt
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
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=150 MAX_LINKS=200 CROWD=${CROWD:-1000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  el(){ echo $(( $(date +%s) - t0 )); }
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  ksrv(){ for p in $(lpid $1); do kill $p 2>/dev/null; done; sleep 0.3; for p in $(lpid $1); do kill -9 $p 2>/dev/null; done
    ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p $1 >/dev/null 2>&1 &
    for i in $(seq 1 25); do ip netns exec kh$ID ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  bgslot(){ i=$1; port=$((8451+i)); kp=$((5211+i)); RANDOM=$((142+i*7)); sleep $((i*3))
    while [ ! -e $O/STOPS ] && [ $(el) -lt $((TT-40)) ]; do
      RS=(${BGRS:-20_30_50_80}); RS=(${RS[@]//_/ }); r=${RS[$((RANDOM % 4))]}; d=$((30 + RANDOM % 61)); dir=d; R=-R
      [ $((RANDOM % 10)) -ge 7 ] && { dir=u; R=""; }
      [ $(( $(el) + d )) -gt $((TT-30)) ] && d=$((TT - 30 - $(el))); [ $d -lt 10 ] && break
      s=$(el); timeout -k 5 $((d+25)) ip netns exec ir$ID iperf3 -c 127.0.0.1 -p $port --connect-timeout 5000 -t $d -b ${r}M $R -J > $O/bg$i.tmp 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/bg$i.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$s $(el) slot=$i dir=$dir rate=$r dur=$d mbit=$v rc=$rc" >> $O/bg.txt
      [ "$v" = ERR ] && { cp $O/bg$i.tmp $O/tests/bgerr-$i-$s.txt; ksrv $kp; }
      sleep $((5 + RANDOM % 16)); done; rm -f $O/bg$i.tmp; }
  for i in $(seq 0 $((NBG-1))); do bgslot $i & done
  ( t=40; while [ $t -lt $((TT-30)) ]; do while [ $(el) -lt $t ]; do sleep 1; done
      [ -e $O/STOPS ] && break
      DIRF=-R; [ $(( (t/20) % 2 )) = 1 ] && DIRF=""; [ "${UPONLY:-0}" = 1 ] && DIRF=""; [ "${DNONLY:-0}" = 1 ] && DIRF=-R; ksrv 5202; s=$(date +%s.%N)
      SSP=; if [ "${SSMON:-0}" = 1 ]; then ( e=$(( $(date +%s) + 15 )); while [ $(date +%s) -lt $e ]; do for n in ir kh; do echo "@ $n $(date +%s.%N)"; ip netns exec $n$ID ss -tinH state established "( ! dst 127.0.0.1 or sport = :8444 or dport = :5202 )" 2>/dev/null; done; sleep 0.2; done | gzip > $O/tests/ss$t.gz ) & SSP=$!; fi
      FZP=; if [ "${FZ:-0}" != 0 ]; then python3 /opt/hs2lab/dr257/fz.py $ID $O/fz.txt ${FZB:-1200000} $FZ ${FZN:-0} & FZP=$!; sleep 1; fi; timeout -k 5 45 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 -P 1 -t 12 $DIRF -J > $O/tests/t$t.json 2>&1; rc=$?
      v=$(python3 -c "import json;d=json.load(open('$O/tests/t$t.json'));print(round(d['end']['sum_received']['bits_per_second']/1e6), [round(x['sum']['bits_per_second']/1e6) for x in d['intervals']])" 2>/dev/null || echo ERR)
      [ -n "$FZP" ] && wait $FZP; [ -n "$SSP" ] && wait $SSP; echo "$t $(el) start=$s dir=${DIRF:-up} test_mbit=$v rc=$rc" >> $O/tests.txt
      for n in ir kh; do ip netns exec $n$ID tc -s qdisc show > $O/tests/tc$n$t.txt 2>&1; ip netns exec $n$ID tc filter show dev $(ip netns exec $n$ID ip -o link | awk -F': ' '$2!~/^lo/{print $2; exit}' | cut -d@ -f1) >> $O/tests/tc$n$t.txt 2>&1; done
      t=$((t+${TI:-20})); done
      for p in $(lpid 5202); do kill $p 2>/dev/null; done ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=dr257 bash $H/run141.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
tail -1 $O/run.out > $O/result.json
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -i -E 'dl243|rescue|stripe|upg|autowin|smux win|dl240' | tail -3000 > $O/$s.sel.txt; done
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -c -E 'panic|fatal error' > $O/$s.panics; done
sleep 2; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
LK=(/opt/hs2lab/lab.lock /opt/hs2lab/lab.slot1 /opt/hs2lab/lab.slot2); MYLOCK=${LK[0]}
for p in $(pidof iperf3); do [ $(stat -c %Y /proc/$p) -ge $(stat -c %Y $O/env.txt) ] || continue
  ls -l /proc/$p/fd 2>/dev/null | grep -q -E "$MYLOCK\$" && { echo "kill leaked iperf3 $p" >> $O/actions.txt; kill $p; }; done
rm -rf $W
# dr256: reap any iperf3 this run left behind (bg servers re-spawned by ksrv), by exact PID, own unit cgroup only
sleep 3; for p in $(pidof iperf3); do grep -q -E "dr257[a-z0-9]*.service" /proc/$p/cgroup 2>/dev/null && { echo "reap iperf3 $p" >> $O/actions.txt; kill $p; }; done

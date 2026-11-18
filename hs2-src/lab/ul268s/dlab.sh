#!/bin/bash
# dl269: prod-like 12-path lab, single-stream DL tests under many concurrent bg users (from dr257 dlab.sh)
set -u
H=/opt/hs2lab/ul268s/h; O=${O:?}
SB=${SB:?}; SK=${SK:-$SB}; TT=${TT:-170}; NBG=${NBG:-30}
export P3_OUT=$O; mkdir -p $O/logs $O/tests
WD=/tmp/ul268w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="${XENV:-} ${IRX:-} HS2_PPROF=127.0.0.1:6060"; KHE="${XENV:-} ${KHX:-}"
echo "edge=$(sha256sum $SB|cut -c1-8) exit=$(sha256sum $SK|cut -c1-8) TT=$TT NBG=$NBG start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE lab=$LAB_ID SHARED=${SHARED_DN:-}" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":130,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=${DLY:-37ms} QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=0
J="${JIT:-1500us} 25% distribution normal"
B=(${LOSSB:-0.1_0.3_0.2_0.5_0.1_0.3_0.2_0.8_0.4_0.2_1.3_0.1}); B=(${B[@]//_/ })
X=(A B C D E F G H I J K L)
for k in $(seq 0 11); do export PATH_${X[$k]}_NETEM="delay ${DLY:-37ms} $J loss random ${B[$k]}%"; done
export NETEM_EVENTS=""
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
cp $H/agg.py $O/agg.py
python3 -u $O/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" > $O/agg.err 2>&1 &
AGG=$!
BGS=""; for i in $(seq 1 $NBG); do BGS="$BGS d:1"; done
export T=${TT}s HEAVY_SECS=12 HEAVY_OMIT=0 HEAVY_DELAY=5 BG_SECS=1 BG="$BGS" BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=130 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=12 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=${PPROF_ON:-0} PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  sleep 2; t0=$(date +%s); echo $t0 > $O/t0
  [ "${PDARK:-0}" = 1 ] && python3 $H/pathdark.py $ID $O/dark.txt $O/t0 ${PD_START:-30} ${PD_EVERY:-18} ${PD_DUR:-8} 12 $TT &
  bash $H/mon.sh $ID $O/mon.txt & echo $! > $O/mon.pid
  el(){ echo $(( $(date +%s) - t0 )); }
  lpid(){ ip netns exec kh$ID ss -tlnpH "sport = :$1" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u; }
  ksrv(){ for p in $(lpid $1); do kill $p 2>/dev/null; done; sleep 0.3; for p in $(lpid $1); do kill -9 $p 2>/dev/null; done
    ip netns exec kh$ID iperf3 -s -B 127.0.0.1 -p $1 >/dev/null 2>&1 &
    for i in $(seq 1 25); do ip netns exec kh$ID ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && break; sleep 0.2; done; sleep 0.2; }
  bgslot(){ i=$1; port=$((8451+i)); kp=$((5211+i)); RANDOM=$((142+i*7)); sleep $((i % 10))
    while [ ! -e $O/STOPS ] && [ $(el) -lt $((TT-25)) ]; do
      RS=(${BGRS:-5_10_20_30_50_5_10_20_50_0}); RS=(${RS[@]//_/ }); r=${RS[$((RANDOM % ${#RS[@]}))]}; d=$((30 + RANDOM % 61)); dir=d; R=-R
      [ $((RANDOM % 10)) -ge 8 ] && { dir=u; R=""; }
      [ $(( $(el) + d )) -gt $((TT-20)) ] && d=$((TT - 20 - $(el))); [ $d -lt 10 ] && break
      s=$(el); timeout -k 5 $((d+25)) ip netns exec ir$ID iperf3 -c 127.0.0.1 -p $port --connect-timeout 5000 -t $d -b ${r}M $R -J > $O/bg$i.tmp 2>&1; rc=$?
      v=$(python3 -c "import json;print(round(json.load(open('$O/bg$i.tmp'))['end']['sum_received']['bits_per_second']/1e6))" 2>/dev/null || echo ERR)
      echo "$s $(el) slot=$i dir=$dir rate=$r dur=$d mbit=$v rc=$rc" >> $O/bg.txt
      [ "$v" = ERR ] && ksrv $kp
      sleep $((2 + RANDOM % 6)); done; rm -f $O/bg$i.tmp; }
  for i in $(seq 0 $((NBG-1))); do bgslot $i & done
  ( t=40; while [ $t -lt $((TT-25)) ]; do while [ $(el) -lt $t ]; do sleep 1; done
      [ -e $O/STOPS ] && break
      ksrv 5202; s=$(date +%s.%N)
      timeout -k 5 45 ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 --connect-timeout 5000 -P 1 -t 12 ${DIRF--R} -J > $O/tests/t$t.json 2>&1; rc=$?
      v=$(python3 -c "import json;d=json.load(open('$O/tests/t$t.json'));print(round(d['end']['sum_received']['bits_per_second']/1e6), [round(x['sum']['bits_per_second']/1e6) for x in d['intervals']])" 2>/dev/null || echo ERR)
      echo "$t $(el) start=$s dir=${DIRN:-down} test_mbit=$v rc=$rc" >> $O/tests.txt
      ip netns exec kh$ID tc -s qdisc show dev ifbk$ID > $O/tests/ifb$t.txt 2>&1
      t=$((t+${TI:-13})); done
      for p in $(lpid 5202); do kill $p 2>/dev/null; done ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=ul268s bash $H/run141.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 6
kill $(cat $O/mon.pid) 2>/dev/null
for s in ir kh; do cp $W/$s.log $O/logs/$s.full.log 2>/dev/null && gzip -f $O/logs/$s.full.log; done
tail -1 $O/run.out > $O/result.json
zcat $O/logs/kh.full.log.gz 2>/dev/null | grep -a -E 'dl259|dl269' > $O/kh.dl.txt
for s in ir kh; do zcat $O/logs/$s.full.log.gz 2>/dev/null | grep -a -c -E 'panic|fatal error' > $O/$s.panics; done
sleep 2; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
cp $W/ping.txt $O/ping.txt 2>/dev/null; rm -rf $W
sleep 3; for p in $(pidof iperf3); do grep -q -E "ul268r[a-z0-9]*.service" /proc/$p/cgroup 2>/dev/null && { echo "reap iperf3 $p" >> $O/actions.txt; kill $p; }; done

#!/bin/bash
# rs145.sh TAG (A* base: HS2_MCONN_RESCUE=1 both sides; B* + JOINPICK RETRY SCALE both sides).
# Production-like rescue: 13 paths (~90 ms, jitter, base loss), crowd 6000 with churn (life 40 s),
# call 2 mbit, game, heavy down -P4 at 30 s and up -P2 at 120 s. Events (all paths of the group
# dark at once, 100 % loss): t=60 "ip1" paths 0,1,2 for 50 s; t=140 "ip2" paths 3..8 for 50 s.
# Out: /opt/hs2lab/rs145/runs/TAG/{resc.txt,proc.txt,fails.txt,min.txt,result.json}.
# Run: labslot.sh --all bash rs145.sh TAG.  Lab copy of run.sh: 192.168.(50+k) per path (k<=12).
set -u
TAG=$1; O=/opt/hs2lab/rs145/runs/$TAG; L=/opt/hs2lab/rs145/labsrc/lab
SB=${SB:-/opt/hs2lab/rs145/hs2}; TT=${TT:-210}
rm -rf $O; mkdir -p $O
WD=/tmp/rs145w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_RESCUE=1"; KHE="$IRE"
case $TAG in B*) X=" HS2_MCONN_RESCUE_JOINPICK=1 HS2_MCONN_RESCUE_RETRY=1 HS2_MCONN_RESCUE_SCALE=1"; IRE="$IRE$X"; KHE="$KHE$X";; esac
echo "bin=$(sha256sum $SB|cut -c1-8) TT=$TT start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":260,\"min_links\":200,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1
J="1500us 25% distribution normal"; JU=${J// /_}
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2 0.3 0.1 0.4)
XL=(A B C D E F G H I J K L M)
for k in $(seq 0 12); do export PATH_${XL[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
base(){ echo "$1:$2:delay_45ms_${JU}_loss_random_${B[$2]}%"; }
bh(){ echo "$1:$2:delay_45ms_${JU}_loss_random_100%"; }
EV="$(bh 60 0) $(bh 60 1) $(bh 60 2) $(base 110 0) $(base 110 1) $(base 110 2)"
EV="$EV $(bh 140 3) $(bh 140 4) $(bh 140 5) $(bh 140 6) $(bh 140 7) $(bh 140 8)"
EV="$EV $(base 190 3) $(base 190 4) $(base 190 5) $(base 190 6) $(base 190 7) $(base 190 8)"
export NETEM_EVENTS="$EV"
FIFO=$O/samples.fifo; rm -f $FIFO; mkfifo $FIFO
python3 -u /opt/hs2lab/rs145/agg.py "$FIFO" "$O/min.txt" "$O/fails.txt" "$O/lat.txt" > $O/agg.err 2>&1 &
AGG=$!
export T=${TT}s HEAVY_SECS=15 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=200 MAX_LINKS=260 CROWD=6000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=13 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 PROBE_EXTRA="-crowdlife 40s -samplelog $FIFO"
( for i in $(seq 1 120); do ip netns exec ir$ID bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break; sleep 0.5; done
  t0=$(date +%s); echo $t0 > $O/t0; el(){ echo $(( $(date +%s) - t0 )); }
  hpid(){ for p in $(ip netns pids $1$ID); do [ "$(cat /proc/$p/comm 2>/dev/null)" = "$2" ] && echo $p; done; }
  ( while [ ! -e $O/STOPS ]; do t=$(el); [ $t -gt $((TT+30)) ] && break
      for s in "ir hs2" "kh hs2kh"; do set -- $s; p=$(hpid $1 $2 | head -1); [ -n "$p" ] || continue
        echo "$t $1 rss_kb=$(awk '/VmRSS/{print $2}' /proc/$p/status)" >> $O/proc.txt; done
      sleep 10; done ) &
  ( while [ $(el) -lt 30 ]; do sleep 1; done
    ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 40 -R -J > $O/hv1.json 2>&1
    while [ $(el) -lt 120 ]; do sleep 1; done
    ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 2 -t 40 -J > $O/hv2.json 2>&1 ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SB IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rs145 bash $L/run.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 3
for s in ir kh; do grep -a -E 'mconn rescue' $W/$s.log | tail -1 > $O/resc-$s.txt; grep -a -c 'rescue-trace' $W/$s.log > $O/trace-n-$s.txt
  grep -a 'rescue-trace' $W/$s.log | gzip > $O/trace-$s.gz; done
cp $W/netemevents.txt $O/ 2>/dev/null
tail -1 $O/run.out > $O/result.json
sleep 3; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
rm -rf $W

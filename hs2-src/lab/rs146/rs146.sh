#!/bin/bash
# rs146.sh TAG — production topology (run.sh TOPO146=1: 6 Iran IPv4 x 2 kharej IPv4 + 1 IPv6 = 13 paths,
# so a whole-Iran-IP outage darkens 2 paths at once). Arms by TAG letter, all with HS2_MCONN_RESCUE=1:
#   A base, J +JOINPICK, S +SCALE, R +RETRY, B all three (both sides); X = R, Y = B on the rs146 binary.
#   M1 / M2 (NEWB, OLDB env): mixed versions, default env (M1 new edge + old exit, M2 old edge + new exit).
# Crowd 6000 (life 40 s), call 2 mbit, game, heavy down -P4 t20-60, up -P2 t80-120. TT=150.
#   ev1 t40 Iran IP1 dark (k0,k2); t42 cascade: Iran IP5+IP6 dark (k9..k12) = the fresh other-IP links
#       the t41.5 rescue joins go to die right after the join (RETRY test); all back at t80.
#   ev2 t95 Iran IPs 2,3,4 dark (k3..k8, 6 paths) until t135.
set -u
TAG=$1; O=/opt/hs2lab/rs146/runs/$TAG; L=/opt/hs2lab/rs146/labsrc/lab
SB=${SB:-/opt/hs2lab/rs145/hs2}; TT=${TT:-150}
rm -rf $O; mkdir -p $O
WD=/tmp/rs146w; W=$WD$LAB_ID; ID=$LAB_ID
IRE="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_RESCUE=1"
case $TAG in
  J*) X=" HS2_MCONN_RESCUE_JOINPICK=1";; S*) X=" HS2_MCONN_RESCUE_SCALE=1";; R*) X=" HS2_MCONN_RESCUE_RETRY=1";;
  B*|Y*) X=" HS2_MCONN_RESCUE_JOINPICK=1 HS2_MCONN_RESCUE_RETRY=1 HS2_MCONN_RESCUE_SCALE=1";; X*) X=" HS2_MCONN_RESCUE_RETRY=1";;
  M*) X="";; *) X="";; esac
# X / Y: R / B with the rs146 binary (silent-join retry); M1 new edge + rel63c exit, M2 rel63c edge + new exit
case $TAG in X*|Y*) SB=/opt/hs2lab/rs146/hs2new;; esac
SBK=$SB; case $TAG in M1*) SB=$NEWB; SBK=$OLDB;; M2*) SB=$OLDB; SBK=$NEWB;; esac
IRE="$IRE$X"; KHE="$IRE"
echo "bin=$(sha256sum $SB|cut -c1-8) binkh=$(sha256sum $SBK|cut -c1-8) TT=$TT id=$ID cpus=$(taskset -pc $$|awk '{print $NF}') start=$(date -u +%FT%TZ) IRE=$IRE KHE=$KHE" > $O/env.txt
export KHAREJ_EXTRA="\"max_links\":260,\"min_links\":200,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export HS2_LB_FAST=0 LB_V6=1 TOPO146=1 KEEP_BACKLOG=1
J="1500us 25% distribution normal"; JU=${J// /_}
B=(0.1 0.3 0.2 0.5 0.1 0.3 0.2 0.8 0.4 0.2 0.3 0.1 0.4)
XL=(A B C D E F G H I J K L M)
for k in $(seq 0 12); do export PATH_${XL[$k]}_NETEM="delay 45ms $J loss random ${B[$k]}%"; done
base(){ echo "$1:$2:delay_45ms_${JU}_loss_random_${B[$2]}%"; }
bh(){ echo "$1:$2:delay_45ms_${JU}_loss_random_100%"; }
EV=""; for k in 0 2; do EV="$EV $(bh 40 $k)"; done
for k in 9 10 11 12; do EV="$EV $(bh 42 $k)"; done
for k in 0 2 9 10 11 12; do EV="$EV $(base 80 $k)"; done
for k in 3 4 5 6 7 8; do EV="$EV $(bh 95 $k)"; done
for k in 3 4 5 6 7 8; do EV="$EV $(base 135 $k)"; done
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
  ( while [ $(el) -lt 20 ]; do sleep 1; done
    ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 4 -t 40 -R -J > $O/hv1.json 2>&1
    while [ $(el) -lt 80 ]; do sleep 1; done
    ip netns exec ir$ID iperf3 -c 127.0.0.1 -p 8444 -P 2 -t 40 -J > $O/hv2.json 2>&1 ) &
  wait ) > /dev/null 2>&1 &
env BIN=$SB BIN_KH=$SBK IR_ENV="$IRE" KH_ENV="$KHE" HEAVY=down HEAVY_P=1 RUNTAG=rs146 bash $L/run.sh > $O/run.out 2> $O/run.err
touch $O/STOPS; sleep 3
for s in ir kh; do grep -a -E 'mconn rescue' $W/$s.log | tail -1 > $O/resc-$s.txt
  grep -a -E 'rescue.*(retry|joinpick|pick)' $W/$s.log | tail -50 > $O/r145-$s.txt; done
grep -a -c 'lb' $W/kh.log > /dev/null; cp $W/netemevents.txt $O/ 2>/dev/null
tail -1 $O/run.out > $O/result.json
sleep 3; kill $AGG 2>/dev/null; rm -f $FIFO
echo "done $(date -u +%FT%TZ)" > $O/DONE
rm -rf $W

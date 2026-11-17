#!/bin/bash
# d95.sh TAG ARM N SC (bh95: HS2_LB_SILENT quiet soak + per-event crowd failures; from bh93/d93.sh)
# own labsrc copy (probe: crowd fail_t timestamps + -crowdchurn; PAIR_EVENTS PROBd removes a rule)
#   ARM S0: key off both sides   S1: HS2_LB_SILENT=1 both sides
#   SC Q: 1800 s lossy-alive soak (0.8 % Iran IP E + 2.5 % pair 51.1<-49.2 steady; every 180 s a 3 % episode on
#         path 49.2->53.1 (25 s) and a 10 % episode on Iran IP 54.1 (15 s); heavy users in blocks with idle
#         blocks; crowd 300 with churn 20 s)
#      bI: Iran IP 52.1 blackhole at 30 s (heavy start burst 1-10 s), T 65 s   bP: path 49.2->53.1 blackhole at 30 s
#      bB: Iran IP 52.1 blackhole at 5 s (inside the heavy start burst), T 40 s
set -u
TAG=$1; ARM=$2; N=$3; SC=$4; F=/opt/hs2lab/bh95; L=$F/labsrc/lab; D=/opt/hs2lab/cg86
export P3_OUT=$F/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d95-$TAG; W=$WD${LAB_ID:-}
B=$F/bin/hs2-linux-amd64; BIN=$B; export BIN_KH=$B; IRX=""; KHX=""
case $ARM in
  S0) ;;
  S1) IRX="HS2_LB_SILENT=1"; KHX=$IRX;;
  *) echo "bad arm $ARM"; exit 2;;
esac
CR=${CROWD:-3000}; CC=""
case $SC in
  Q) TT=1800s; CR=300; CC=20s
     PEV="1:1:0.025"
     for k in $(seq 0 9); do b=$((k*180)); PEV="$PEV $((b+60)):3:0.03 $((b+85)):3:0.03d $((b+150)):a4:0.1 $((b+165)):a4:0.1d"; done
     python3 $F/mksoak.py ${SEED:-95} > $P3_OUT/users.txt;;
  bI) TT=65s; PEV="1:1:0.025 30:a2:1";;
  bP) TT=65s; PEV="1:1:0.025 30:3:1";;
  bB) TT=40s; PEV="1:1:0.025 5:a2:1";;
  *) echo "bad sc $SC"; exit 2;;
esac
export T=$TT
if [ "$SC" != Q ] && [ "$N" -gt 0 ]; then python3 $D/mkusers.py $N ${SEED:-95} 30 > $P3_OUT/users.txt; fi
[ -s $P3_OUT/users.txt ] && export HV_USERS=$P3_OUT/users.txt
echo "ARM=$ARM N=$N SC=$SC BIN=$BIN KHX=$KHX IRX=$IRX SEED=${SEED:-95} CROWD=$CR CHURN=$CC PEV=$PEV" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 0.3%" PATH_E_NETEM="loss random 0.8%"
export KH2=1 PAIR_EVENTS="$PEV" LOSS_EVENTS=""
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=$CR CROWD_CHURN=$CC REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 SPTBIN=$D/spt
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run86b.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM N=$N SC=$SC" > $P3_OUT/meta.txt
rm -rf "$W"

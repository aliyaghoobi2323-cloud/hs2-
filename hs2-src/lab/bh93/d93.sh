#!/bin/bash
# d93.sh TAG ARM N SC (bh93: HS2_LB_SILENT A/B; from fc90/d90.sh)
# d86.sh TAG ARM N SC: cg86 mesh lab (5 Iran IPs x 2 kharej IPv4 = 10 paths, reverse l3mtcp, 90 ms rtt,
# per-Iran-IP loss 0.2-1 %, pair 192.168.51.1<-192.168.49.2 genuinely lossy 2.5 % like 37.32<->91.107)
# + crowd/call/game + N heavy users. Edge hs2C (cg86), exit rel58.
#   ARM C0: guard off   C1: HS2_LB_MULTI_CONFIRM=1 on the edge
#   SC q: quiet   p: at 12 s one more pair (49.2 -> 192.168.53.1) drops 6 %   o: at 12 s Iran IP 192.168.52.1 30 % loss
set -u
TAG=$1; ARM=$2; N=$3; SC=$4; D=/opt/hs2lab/cg86; L=$D/labsrc/lab; F=/opt/hs2lab/bh93
export P3_OUT=$F/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/d93-$TAG; W=$WD${LAB_ID:-}
IRX=""; KHX=""; BIN=$D/hs2C; export BIN_KH=$D/hs2R58
B93=$F/bin/hs2-linux-amd64
case $ARM in
  S0) BIN=$B93; BIN_KH=$B93;;
  S1) BIN=$B93; BIN_KH=$B93; IRX="HS2_LB_SILENT=1"; KHX=$IRX;;
  M1) BIN=$D/hs2R58; BIN_KH=$B93; KHX="HS2_LB_SILENT=1";;
  M2) BIN=$B93; BIN_KH=$D/hs2R58; IRX="HS2_LB_SILENT=1";;
  *) echo "bad arm $ARM"; exit 2;;
esac
PEV="1:1:0.025"; LEV=""; TT=30s
case $SC in
  q) ;;
  p) PEV="1:1:0.025 12:3:0.06";;
  o) PEV="1:1:0.025 12:a2:0.3";;
  b|bB) PEV="1:1:0.025 12:a2:1";;
  oB) PEV="1:1:0.025 12:a2:0.3";;
  pB) PEV="1:1:0.025 12:3:0.06";;
  bL) PEV="1:1:0.025 30:a2:1"; TT=52s;;
  oL) PEV="1:1:0.025 30:a2:0.3"; TT=52s;;
  pL) PEV="1:1:0.025 30:3:0.06"; TT=52s;;
  sL) PEV="1:1:0.025 30:3:1"; TT=52s;;
  sQ) PEV="1:1:0.025 12:3:1";;
  *) echo "bad sc $SC"; exit 2;;
esac
export T=$TT
if [ "$N" -gt 0 ]; then python3 $D/mkusers.py $N ${SEED:-86} 30 > $P3_OUT/users.txt; export HV_USERS=$P3_OUT/users.txt; fi
echo "ARM=$ARM N=$N SC=$SC BIN=$BIN BIN_KH=$BIN_KH KHX=$KHX IRX=$IRX SEED=${SEED:-86} PEV=$PEV LEV=$LEV" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 0.3%" PATH_E_NETEM="loss random 0.8%"
export KH2=1 PAIR_EVENTS="$PEV" LOSS_EVENTS="$LEV"
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=5 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 SPTBIN=$D/spt
env BIN=$BIN BIN_KH=$BIN_KH IR_ENV="$IRX" KH_ENV="$KHX" LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run86b.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM N=$N SC=$SC" > $P3_OUT/meta.txt
rm -rf "$W"

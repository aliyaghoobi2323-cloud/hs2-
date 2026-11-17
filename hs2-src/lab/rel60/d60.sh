#!/bin/bash
# d60.sh TAG ARM MODE: rel60 A/B on d59's lab (rel59 A/B design). ARM P rel59 both, Q rel60 both,
# M dl79 edge + rel60 kharej. Run via seq60.sh on labslot --all.
# d78.sh TAG ARM MODE(d1|d8|u1|u8): dl75's lab (reverse l3mtcp, 10 paths 90 ms, loss
# dl78: C = floor 6 + gate 2 (dl77 "H"), M = floor 4 + gate 2.
# A: hs2D77 (gate74 + inert dl76/dl77 keys), B: floor 6 (dl76), C: floor 6 + queue gate (dl77); exit: rel58
# 0.2-2 %, crowd 3000+call+game, one iperf3 -R) plus per-second ss -ti of the
# exit's tunnel link sockets only (src 192.168/16, not the crowd's inner sockets)
# when SS=1, and for ARM T/TB, HS2_DL76_TRACE on both sides. B/C: B_IR/C_IR env.
set -u
TAG=$1; ARM=$2; MD=$3; D=/opt/hs2lab/dl76; R=/opt/hs2lab/rel60; R59=/opt/hs2lab/rel59; L=/opt/hs2lab/r54/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/r60-$TAG; W=$WD$LAB_ID
IRX=""; KHX=""; BIN=$D/hs2D77; export BIN_KH=$D/hs2R58
case $ARM in
  P) BIN=$R59/hs2R59; export BIN_KH=$R59/hs2R59;;   # rel59 both sides
  Q) BIN=$R/hs2R60; export BIN_KH=$R/hs2R60;;       # rel60 both sides
  M) BIN=$D/hs2D79; export BIN_KH=$R/hs2R60;;       # dl79 edge + rel60 kharej (mixed)
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$BIN IRX=$IRX KHX=$KHX" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
if [ "${SS:-0}" = 1 ]; then
( for i in $(seq 1 40); do sleep 1; echo "@T $(date +%s.%N)"; ip netns exec kh$LAB_ID ss -tinH state established '( src 192.168.0.0/16 )' 2>/dev/null; done > $P3_OUT/ss.txt ) &
SP=$!
fi
( for i in $(seq 1 80); do sleep 0.5; f=$(ls /run/hs2/tmp-r60-$TAG*-ir.json.status.json 2>/dev/null|head -1); [ -n "$f" ] && echo "$(date +%s.%N) $(grep -o '"aw_floor_[a-z]*": *[0-9]*' $f | tr '
' ' ')"; done > $P3_OUT/floor.txt ) &
FP=$!
env BIN=$BIN BIN_KH=${BIN_KH:-$BIN} IR_ENV="$IRX" KH_ENV="$KHX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
[ "${SS:-0}" = 1 ] && kill $SP 2>/dev/null
kill $FP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
rm -rf $W

#!/usr/bin/env bash
# run1.sh NAME DIR CAR FARM VAR — s48 mode matrix (docs/reality-architecture.md §11.7)
#   DIR d (direct: kharej listens) | r (reverse: iran listens); CAR mtcp|l3mtcp|tls
#   FARM tls|reality|both (every listener IP); VAR:
#     m  crowd 3000 + call + game + heavy down x8, 75 % of the links killed at 12 s (tls carrier: its one link)
#     s  single-connection download, nothing else
#     k  as m without the kill, cold dialer restart at 16 s (CK=1)
#     sc scale: 6 IPs 40/20/15/10/10/5, per-IP keys + 2 targets, path c 30 % loss 12-32 s
set -u
export ROT=""; case $1 in *naive*) ROT=naive;; *two*) ROT=two;; esac
NAME=$1; DIRM=$2; CAR=$3; export FARM=$4; V=$5; D=/opt/hs2lab/r48
L=$D/labsrc/hs2-src/lab
export P3_OUT=$D/res/$NAME; rm -rf $P3_OUT; mkdir -p $P3_OUT
COMMON="HS2_SHAPE_FAST=1 HS2_SHAPE_SMALL=1 HS2_L3_LANE=1 HS2_HEAVY_BACKLOG=1 HS2_L3_DUP=1 HS2_MCONN=lite HS2_FAST_LINK=1 HS2_MCONN_BATCH=1 HS2_SHAPE_BULK=1 HS2_QUIET_IDLE=1 HS2_TLS_RESUME=1 HS2_AUTH_PAD=1 HS2_COVER_NGINX=1 HS2_MCONN_DEADLINE_FIX=1 HS2_MCONN_CLOSE_HS=1 HS2_LB_AUTO2=1 HS2_MCONN_CALL=1 HS2_MCONN_CALL_AVOID=1 HS2_MCONN_STRIPE=8 HS2_LB_JUDGE_DATA=1 HS2_LB_FAST_VERDICT=1 HS2_LB_RESCUE=1 HS2_LB_RESCUE_CALLFREE=1 ${XENV:-}"
IRP="HS2_SMUX_AUTOWIN=1 HS2_SMUX_AUTOWIN_REL=1 HS2_SMUX_AUTOWIN_SHARE_MB=32 HS2_STRIPE_AW_SOLO=1 HS2_STRIPE_AW_HEAVY=2 HS2_LB_FAST_RECOVER=1 HS2_LB_START_GRACE_S=20"
KHP="HS2_SMUX_AUTOWIN=1 HS2_SMUX_AUTOWIN_REL=1 HS2_SMUX_AUTOWIN_SHARE_MB=32 HS2_MCONN_STRIPE_JUMP_MS=1500 HS2_LB_FAST_RECOVER=1 HS2_LB_START_GRACE_S=20 HS2_LB_FILL_PCT=25 HS2_LB_SPARE_PER_ROUND=3 HS2_LB_REC_CONFIRM=1"
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
PATHS=3; KP=75; [ $CAR = tls ] && { PATHS=1; KP=100; KHAREJ_EXTRA=""; }
LD=0; REV=1; [ $DIRM = d ] && { LD=1; REV=0; }
LOSS=0; HV=down; HP=8; T=30s; KA=12; CR=3000; CALLV=2mbit; GM=1; CKV=0; SC=0; LW=""; LE=""
case $V in
  m) ;;
  s) HP=1; T=13s; KA=0; CR=0; CALLV=""; GM=0;;
  k) KA=0; CKV=1;;
  rs) KA=16;;   # rotate-sid test: ROT=two|naive at 10 s, 75 % kill at 16 s
  sc) PATHS=6; SC=1; LW=40,20,15,10,10,5; LE="12:2:0.3 32:2:-"; T=45s; KA=0; HP=4;;
esac
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin MODE=$CAR EMU=kernel RATE=1000mbit DELAY=37ms QUEUE=100ms
export T BULK=0 CALL=$CALLV GAME=$GM MIN_LINKS=100 MAX_LINKS=150 CROWD=$CR REVERSE=$REV LB_DIRECT=$LD NOWARM=1 WORKDIR=/tmp/r48w$NAME PATHS LB_MODE=auto LOSS KILL_AT=$KA KILL_PCT=$KP
export HEAVY_CTL=direct PPROF=0 CK=$CKV SCALE=$SC LB_WEIGHTS=${LW:-50,50} LOSS_EVENTS="$LE"
env BIN=$D/w3.sh BIN_KH=$D/w3.sh IR_ENV="$COMMON HS2_LINK_PROBATION=1 HS2_PARTIAL_REFILL=1 HS2_PICK_LANE=on HS2_QUEUE_CTL_UP=1 $IRP" KH_ENV="$COMMON HS2_QUEUE_CTL=1 $KHP" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$NAME bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
for p in $(cat $P3_OUT/tpids 2>/dev/null); do kill $p 2>/dev/null; done
rm -rf /tmp/r48w$NAME
echo "LAB_ID=${LAB_ID:-} DIR=$DIRM CAR=$CAR FARM=$FARM V=$V" > $P3_OUT/meta.txt

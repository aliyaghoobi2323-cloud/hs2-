#!/usr/bin/env bash
# l16: like job.sh (IP b 7% loss 20..60 s, crowd 2000<->3000 churn 60 s, T=260 s) but
# base = production 14k binary + production keys; fix = lb-ready16 + SPARE_FIX/SPARE_MAX/START_GRACE/FAST_RECOVER.
# One labslot slot per invocation (REPS=..); WORKDIR per slot so 3 slots can run at once.
set -u
OUT=/opt/hs2lab/l16/${RES:-res}; mkdir -p $OUT/logs; LOGDIR=$OUT/logs
L=/opt/hs2lab/l16/lab3g/hs2-src/lab
COMMON="HS2_SHAPE_FAST=1 HS2_SHAPE_SMALL=1 HS2_L3_LANE=1 HS2_HEAVY_BACKLOG=1 HS2_L3_DUP=1 HS2_MCONN=lite HS2_FAST_LINK=1 HS2_MCONN_BATCH=1 HS2_SHAPE_BULK=1 HS2_QUIET_IDLE=1 HS2_TLS_RESUME=1 HS2_AUTH_PAD=1 HS2_COVER_NGINX=1 HS2_MCONN_DEADLINE_FIX=1 HS2_MCONN_CLOSE_HS=1 HS2_LB_AUTO2=1 HS2_MCONN_CALL=1 HS2_MCONN_CALL_AVOID=1"
PROD="HS2_MCONN_STRIPE=8 HS2_MCONN_STRIPE_UP=1 HS2_MCONN_STRIPE_SHARE=1 HS2_MCONN_STRIPE_REL=1 HS2_MCONN_STRIPE_QWIN_MS=8000 HS2_LB_CUT_DRAIN=1 HS2_SMUX_AUTOWIN=1 HS2_SMUX_AUTOWIN_REL=1 HS2_SMUX_AUTOWIN_SHARE_MB=32"
KHONLY="HS2_MCONN_STRIPE_JUMP_MS=1500"
NEW="HS2_LB_SPARE_FIX=1 HS2_LB_SPARE_MAX=${SMAX:-3} HS2_LB_START_GRACE_S=20 HS2_LB_FAST_RECOVER=1"
IRB="$COMMON HS2_LINK_PROBATION=1 HS2_PARTIAL_REFILL=1 HS2_PICK_LANE=on"
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":40,"
export HOME=/root GOPATH=/root/go GOPROXY=off PATH=$PATH:/usr/local/go/bin MODE=l3mtcp EMU=kernel RATE=3000mbit DELAY=40ms QUEUE=100ms
export T=${TT:-260s} BULK=0 CALL=2mbit GAME=1 MIN_LINKS=40 MAX_LINKS=150 CROWD=${CROWD:-3000} REVERSE=1 NOWARM=1 WORKDIR=/tmp/l16s${LAB_ID:-0} LB_SAMPLE=1
export HS2_CROWD_LIFE=${LIFE:-60} HS2_CROWD_VAR=${CVAR:-60}
export PATHS=3 LB_MODE=auto LOSS_EVENTS="${LEV:-20:1:0.07 60:1:-}"
for REP in ${REPS:-1 2}; do
  case $REP in 1|3|5|7) arms="base fix";; *) arms="fix base";; esac
  for a in $arms; do
    case $a in base) BIN=/opt/hs2lab/hs2-lbready14k; ex="";; fix) BIN=/opt/hs2lab/hs2-lbready16; ex="$NEW";; esac
    tag=$a-r$REP
    grep -q "\"tag\":\"$tag\"" $OUT/runs.jsonl 2>/dev/null && continue
    rm -rf $WORKDIR; line=$(env BIN=$BIN IR_ENV="$IRB $PROD $ex" KH_ENV="$COMMON $PROD $KHONLY $ex" LOSS=0.003 HEAVY=down HEAVY_P=1 LOGDIR=$LOGDIR RUNTAG=$tag bash $L/run.sh 2>$LOGDIR/$tag.err | tail -1)
    cp $WORKDIR/heavy.json $LOGDIR/$tag-heavy.json 2>/dev/null
    case "$line" in "{"*) ;; *) line=null ;; esac
    echo "{\"tag\":\"$tag\",\"arm\":\"$a\",\"rep\":$REP,\"slot\":\"${LAB_ID:-}\",\"t\":\"$(date +%T)\",\"res\":$line}" >> $OUT/runs.jsonl
  done
done
touch $OUT/done-${LAB_ID:-0}

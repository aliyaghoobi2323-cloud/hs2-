#!/bin/bash
set -u
R=/opt/hs2lab/dl243; s=$1; BIN=$2; PLAN=$4
rm -rf /tmp/bs201w${LAB_ID:-30}; [ -d $R/$s ] && mv $R/$s $R/$s.old.$(date +%s); mkdir -p $R/$s
echo "$(date -u +%FT%TZ) start $s plan=$PLAN bin=$(sha256sum $BIN|cut -c1-8) xenv=${3:-}" >> $R/seq.log
O=$R/$s SB=$BIN SK=$BIN TT=${TT:-390} TI=10 XENV="${3:-}" bash $R/soak242$PLAN.sh > $R/$s.sh.out 2>&1
echo "$(date -u +%FT%TZ) end $s rc=$?" >> $R/seq.log

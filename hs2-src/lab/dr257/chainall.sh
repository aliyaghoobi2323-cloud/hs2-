#!/bin/bash
# dr258: B (ul257 fixed, forced-stuck uploads) then dl259 DL ABAB
R=/opt/hs2lab/dr257
log(){ echo "$(date -u +%FT%TZ) $*" >> $R/seq.log; }
run(){ s=$1 I=$2 X=$3; mkdir -p $R/$s; log "start $s bin=$(sha256sum $I|cut -c1-8) X=$X $4"
  env $4 O=$R/$s SB=$I SK=$I XENV="HS2_MCONN_RESCUE_RST=1 $X" TT=200 TI=16 bash /opt/hs2lab/labslot.sh --all bash $R/dlab.sh > $R/$s.out 2>&1
  log "end $s rc=$?"; sleep 5; }
for s in B3f B4f; do [ -e $R/STOP2 ] && exit; run $s $R/bin/hs2B3 "" "FZ=4 UPONLY=1"; done
for s in P1 Q1 P2 Q2; do [ -e $R/STOP2 ] && exit
  case $s in P*) run $s $R/bin/hs2B2 "" "DNONLY=1";; Q*) run $s $R/bin/hs2C "HS2_DL259_PATHQ=1" "DNONLY=1";; esac; done
log "chainall done"

#!/bin/bash
# dr258b: heavy-loss (25 % data, 4 s) forced-stuck uploads ABAB, then dl259 DL A/B
R=/opt/hs2lab/dr257
log(){ echo "$(date -u +%FT%TZ) $*" >> $R/seq.log; }
while systemctl is-active -q dr257all; do sleep 3; done
rm -f $R/STOP2
run(){ s=$1 I=$2 X=$3; mkdir -p $R/$s; log "start $s bin=$(sha256sum $I|cut -c1-8) X=$X $4 TT=$5"
  env $4 O=$R/$s SB=$I SK=$I XENV="HS2_MCONN_RESCUE_RST=1 $X" TT=$5 TI=16 bash /opt/hs2lab/labslot.sh --all bash $R/dlab.sh > $R/$s.out 2>&1
  log "end $s rc=$?"; sleep 3; }
for s in AL1 BL1 AL2 BL2; do [ -e $R/STOP2 ] && exit
  case $s in A*) I=$R/bin/hs2I;; B*) I=$R/bin/hs2B3;; esac; run $s $I "" "FZ=4 FZN=4 UPONLY=1" 180; done
for s in P1 Q1 P2 Q2; do [ -e $R/STOP2 ] && exit
  case $s in P*) run $s $R/bin/hs2B2 "" "DNONLY=1" 180;; Q*) run $s $R/bin/hs2C "HS2_DL259_PATHQ=1" "DNONLY=1" 180;; esac; done
log "chain2 done"

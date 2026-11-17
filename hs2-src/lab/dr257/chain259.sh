#!/bin/bash
R=/opt/hs2lab/dr257
log(){ echo "$(date -u +%FT%TZ) $*" >> $R/seq.log; }
while systemctl is-active -q dr257fz1; do sleep 5; done
for s in ${ORDER:?}; do
  [ -e $R/STOP2 ] && break
  a=${s:0:1}; X=""; I=$R/bin/hs2B2
  case $a in Q) I=$R/bin/hs2C; X="HS2_DL259_PATHQ=1";; esac
  mkdir -p $R/$s; log "start $s bin=$(sha256sum $I|cut -c1-8) X=$X"
  O=$R/$s SB=$I SK=$I XENV="HS2_MCONN_RESCUE_RST=1 $X" TT=${TT:-200} bash /opt/hs2lab/labslot.sh --all bash $R/dlab.sh > $R/$s.out 2>&1
  log "end $s rc=$?"; sleep 5
done
log "chain259 done"

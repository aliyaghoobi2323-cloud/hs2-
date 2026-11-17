#!/bin/bash
R=/opt/hs2lab/dr257
log(){ echo "$(date -u +%FT%TZ) $*" >> $R/seq.log; }
for s in ${ORDER:?}; do
  [ -e $R/STOP2 ] && break
  a=${s:0:1}; X=""; I=$R/bin/hs2I
  case $a in B) I=$R/bin/hs2B2;; esac
  mkdir -p $R/$s; log "start $s bin=$(sha256sum $I|cut -c1-8) X=$X"
  O=$R/$s SB=$I SK=$I XENV="HS2_MCONN_RESCUE_RST=1 ${TRX:-} $X" TT=${TT:-260} bash /opt/hs2lab/labslot.sh --all bash $R/dlab.sh > $R/$s.out 2>&1
  log "end $s rc=$?"; sleep 5
done
log "chain done"

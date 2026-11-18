#!/bin/bash
# bin280 default (SR) vs bin280 HS2_UL268_RESCUE=0 (SU), interleaved
R=/opt/hs2lab/dc254
for s in SU8:0 SR8:1 SR9:1 SU9:0 SU10:0 SR10:1; do
  [ -e $R/STOP ] && exit 0
  n=${s%%:*}; v=${s#*:}
  if [ $v = 0 ]; then X="HS2_UL268_RESCUE=0"; else X=""; fi
  SEGS="$n:b280" XENV="$X" bash $R/h2/sk280e.sh
done
echo "$(date -u +%FT%TZ) sk286p done" >> $R/seq.log

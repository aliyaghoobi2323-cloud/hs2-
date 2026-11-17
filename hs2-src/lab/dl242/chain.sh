#!/bin/bash
B=/opt/hs2lab/dl242/bin/hs2R; L="HS2_MCONN_STRIPE_LOG=1"
for a in A1:a R1:a A2:b R2:b; do [ -e /opt/hs2lab/dl242/STOP ] && exit 0
  n=${a%:*}; p=${a#*:}; x="$L"; case $n in R*) x="$L HS2_DL242_RESCUE=1";; esac
  /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/dl242/arm.sh $n $B "$x" $p; done

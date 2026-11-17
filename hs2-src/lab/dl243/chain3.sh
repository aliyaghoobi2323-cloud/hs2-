#!/bin/bash
while systemctl is-active -q dl243chain2; do sleep 5; done
B=/opt/hs2lab/dl243/bin/hs2R; L="HS2_MCONN_STRIPE_LOG=1"
for a in A3:b R3:b R4:b A4:b; do [ -e /opt/hs2lab/dl243/STOP ] && exit 0
  n=${a%:*}; p=${a#*:}; x="$L"; case $n in R*) x="$L HS2_DL243_RESCUE=1";; esac
  /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/dl243/arm.sh $n $B "$x" $p; done

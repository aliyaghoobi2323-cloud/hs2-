#!/bin/bash
B=/opt/hs2lab/dl241/bin/hs2A; L="HS2_MCONN_STRIPE_LOG=1"
for a in A1 B1 A2 B2; do [ -e /opt/hs2lab/dl241/STOP ] && exit 0
  x="$L"; case $a in B*) x="$L HS2_DL240_YOUNG=1";; esac
  /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/dl241/arm.sh $a $B "$x"; done

#!/bin/bash
while systemctl is-active -q dl241chain; do sleep 5; done
[ -e /opt/hs2lab/dl241/STOP ] && exit 0
/opt/hs2lab/labslot.sh --all bash /opt/hs2lab/dl241/arm.sh S1 /opt/hs2lab/dl241/bin/hs2S "HS2_MCONN_STRIPE_LOG=1 HS2_DL240_YOUNG=1 HS2_DL241_SLOWSLOT=1"

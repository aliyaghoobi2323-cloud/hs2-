#!/bin/bash
while systemctl is-active -q dl242chain; do sleep 5; done
[ -e /opt/hs2lab/dl242/STOP ] && exit 0
/opt/hs2lab/labslot.sh --all bash /opt/hs2lab/dl242/arm.sh R1b /opt/hs2lab/dl242/bin/hs2R "HS2_MCONN_STRIPE_LOG=1 HS2_DL242_RESCUE=1" a

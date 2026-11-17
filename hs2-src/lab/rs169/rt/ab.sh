#!/bin/bash
# rs169 retry-heavy A/B on lab1: same binary (hs2-rs169f = 08d35b12), arm I = HS2_MCONN_RESCUE_IDEM=1, arm Z = =0
# (rel66 behaviour), both sides. slot1 held by a dummy; first arm of ORDER gets slot0 (cores 0-6), second slot2.
# usage: ROT=1 TT=480 ORDER="I Z" bash ab.sh
D=/opt/hs2lab/rs169; A=$D/rt/ab; B=$D/hs2-rs169f
# cascade timing (ev169.py EV_CASCADE_AT): rotation 1 reactive (first join), rotation 2 fixed T+1.3 s
CAS=${CAS:-$([ "$ROT" = 1 ] && echo 6 || echo 1.3)}
declare -A XE=([I]="HS2_MCONN_RESCUE_IDEM=1" [Z]="HS2_MCONN_RESCUE_IDEM=0")
systemd-run --unit=rt169-hold$ROT --collect flock /opt/hs2lab/lab.slot1 sleep $((TT+300))
sleep 2
for n in $ORDER; do
  systemd-run --unit=rt169-$ROT$n --collect --setenv=O=$A/r$ROT$n --setenv=SB=$B --setenv=SK=$B \
    --setenv=TT=$TT "--setenv=XENV=${XE[$n]}" --setenv=EV_CASCADE_AT=$CAS /opt/hs2lab/labslot.sh bash $D/rt/rt169.sh
  sleep 8
done

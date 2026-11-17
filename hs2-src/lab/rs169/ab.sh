#!/bin/bash
# rs169 A/B on lab1 (rr166 ab.sh adapted): F = rel66 (90fab61d), R = rs169, both default env (envdef), no extra keys.
# slot1 held by a dummy; arms in slot0 (cores 0-6) and slot2 (cores 13-19). HS2_MCONN_TRACE off.
# usage: ROT=1 TT=1000 ORDER="F R" bash ab.sh  (first arm gets slot0). rep.sh = rr166 rep.sh unchanged.
D=/opt/hs2lab/rs169; A=$D/ab
declare -A BIN=([F]=$D/hs2-rel66 [R]=$D/hs2-rs169)
systemd-run --unit=rs169ab-hold$ROT --collect flock /opt/hs2lab/lab.slot1 sleep $((TT+400))
sleep 2
for n in $ORDER; do
  TRV="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_END_LOG=1 HS2_MCONN_RESCUE_TRACE=1"
  systemd-run --unit=rs169ab-$ROT$n --collect --setenv=O=$A/r$ROT$n --setenv=SB=${BIN[$n]} --setenv=SK=${BIN[$n]} \
    --setenv=TT=$TT --setenv=CROWD=3000 "--setenv=XENV=" \
    "--setenv=TR=$TRV" /opt/hs2lab/labslot.sh bash /opt/hs2lab/rr166/rep.sh
  sleep 8
done

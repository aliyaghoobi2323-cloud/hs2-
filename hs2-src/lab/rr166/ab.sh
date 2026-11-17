#!/bin/bash
# rr166 clean A/B on lab1: F = fn164 (c7adaaf5, key off) vs R = rr166 (fn164 + rr158 rescue-rst, HS2_MCONN_RESCUE_RST=1 both sides).
# slot1 held by a dummy; arms in slot0 (cores 0-6) and slot2 (cores 13-19). HS2_MCONN_TRACE off.
# usage: ROT=1 TT=1000 ORDER="F R" RTRACE=0 bash ab.sh  (first arm gets slot0); RTRACE=1 adds HS2_MCONN_RESCUE_TRACE=1 to R.
D=/opt/hs2lab/rr166; A=$D/ab
declare -A BIN=([F]=$D/hs2-fn164 [R]=$D/hs2-rr166)
declare -A XE=([F]="" [R]="HS2_MCONN_RESCUE_RST=1")
systemd-run --unit=rr166ab-hold$ROT --collect flock /opt/hs2lab/lab.slot1 sleep $((TT+400))
sleep 2
for n in $ORDER; do
  TRV="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_END_LOG=1"
  [ "$n" = R ] && [ "${RTRACE:-0}" = 1 ] && TRV="$TRV HS2_MCONN_RESCUE_TRACE=1"
  systemd-run --unit=rr166ab-$ROT$n --collect --setenv=O=$A/r$ROT$n --setenv=SB=${BIN[$n]} --setenv=SK=${BIN[$n]} \
    --setenv=TT=$TT --setenv=CROWD=3000 "--setenv=XENV=${XE[$n]}" \
    "--setenv=TR=$TRV" /opt/hs2lab/labslot.sh bash $D/rep.sh
  sleep 8
done

#!/bin/bash
# cs194 A/B/C on lab3: rep.sh (cs193) x 3 arms x 2 rotations, all 3 slots per rotation.
# Same binary (hs2-194) all arms, HS2_MCONN_DARKCALL_TRACE=1 everywhere (observe-only timer in A):
# A = off, B = DARKCALL (1000 ms), C = DARKCALL_MS=700. r1 order A B C, r2 order C A B (slots rotate).
D=/opt/hs2lab/cs193; A=$D/ab194; mkdir -p $A
declare -A XE=([A]="HS2_MCONN_DARKCALL_TRACE=1" [B]="HS2_MCONN_DARKCALL_TRACE=1 HS2_MCONN_DARKCALL=1" [C]="HS2_MCONN_DARKCALL_TRACE=1 HS2_MCONN_DARKCALL=1 HS2_MCONN_DARKCALL_MS=700")
TRV="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_END_LOG=1 HS2_MCONN_RESCUE_TRACE=1"
for ROT in 1 2; do
  [ $ROT = 1 ] && ORDER="A B C" || ORDER="C A B"
  for n in $ORDER; do
    systemd-run --unit=cs194ab-$ROT$n --collect --setenv=O=$A/r$ROT$n --setenv=SB=$D/hs2-194 --setenv=SK=$D/hs2-194 \
      --setenv=TT=500 --setenv=CROWD=3000 "--setenv=XENV=${XE[$n]}" "--setenv=TR=$TRV" /opt/hs2lab/labslot.sh bash $D/rep.sh
    sleep 8
  done
  while [ ! -e $A/r${ROT}A/DONE ] || [ ! -e $A/r${ROT}B/DONE ] || [ ! -e $A/r${ROT}C/DONE ]; do sleep 10; done
  sleep 20
done
echo "all done $(date -u +%FT%TZ)" > $A/ALLDONE

#!/bin/bash
# cs193 A/B on lab3 (rep.sh: 13-path production topology, ip1 dark 60 s, 30 % RST, ip2x, flap, loss_all;
# crowd 3000, 30 byte-checked 2 Mbit calls with gap logging, probe call/game; TT 500 x 2 rotations).
# Same binary both arms (cs193 hs2-new); A = defaults, B adds HS2_MCONN_DARKCALL=1 both sides.
# r1: A slot0 / B slot2; r2: B slot0 / A slot2; slot1 held (go test there).
D=/opt/hs2lab/cs193; A=$D/ab; mkdir -p $A
declare -A XE=([A]="" [B]="HS2_MCONN_DARKCALL=1")
TRV="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_END_LOG=1 HS2_MCONN_RESCUE_TRACE=1"
for ROT in 1 2; do
  [ $ROT = 1 ] && ORDER="A B" || ORDER="B A"
  systemd-run --unit=cs193ab-hold$ROT --collect flock /opt/hs2lab/lab.slot1 sleep 1000
  sleep 2
  for n in $ORDER; do
    systemd-run --unit=cs193ab-$ROT$n --collect --setenv=O=$A/r$ROT$n --setenv=SB=$D/hs2-new --setenv=SK=$D/hs2-new \
      --setenv=TT=500 --setenv=CROWD=3000 "--setenv=XENV=${XE[$n]}" "--setenv=TR=$TRV" /opt/hs2lab/labslot.sh bash $D/rep.sh
    sleep 8
  done
  while [ ! -e $A/r${ROT}A/DONE ] || [ ! -e $A/r${ROT}B/DONE ]; do sleep 10; done
  systemctl stop cs193ab-hold$ROT 2>/dev/null; sleep 20
done
echo "all done $(date -u +%FT%TZ)" > $A/ALLDONE

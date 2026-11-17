#!/bin/bash
# cs194 rotation 3 (after ab194.sh): the cs194 "again" fix binary (hs2-194b): B' = DARKCALL 1000, C' = DARKCALL 700,
# plus the full go test ./engine in the middle slot.
D=/opt/hs2lab/cs193; A=$D/ab194
while [ ! -e $A/ALLDONE ]; do sleep 10; done
XB="HS2_MCONN_DARKCALL_TRACE=1 HS2_MCONN_DARKCALL=1"; XC="$XB HS2_MCONN_DARKCALL_MS=700"
TRV="HS2_PPROF=127.0.0.1:6060 HS2_MCONN_STRIPE_LOG=1 HS2_END_LOG=1 HS2_MCONN_RESCUE_TRACE=1"
run(){ systemd-run --unit=cs194ab-3$1 --collect --setenv=O=$A/r3$1 --setenv=SB=$D/hs2-194b --setenv=SK=$D/hs2-194b \
  --setenv=TT=500 --setenv=CROWD=3000 "--setenv=XENV=$2" "--setenv=TR=$TRV" /opt/hs2lab/labslot.sh bash $D/rep.sh; sleep 8; }
run B "$XB"
systemd-run --unit=cs194gotest --collect --setenv=HOME=/root --setenv=GOPATH=/root/go --setenv=GOPROXY=off --setenv=GOFLAGS=-mod=mod \
  /opt/hs2lab/labslot.sh bash -c "cd $D/src194 && /usr/local/go/bin/go test ./engine -count=1 -timeout 30m > $D/gotest194.txt 2>&1; echo rc=\$? >> $D/gotest194.txt"
sleep 8
run C "$XC"
while [ ! -e $A/r3B/DONE ] || [ ! -e $A/r3C/DONE ]; do sleep 10; done
echo "r3 done $(date -u +%FT%TZ)" > $A/R3DONE

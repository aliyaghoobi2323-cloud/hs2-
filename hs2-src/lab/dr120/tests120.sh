#!/bin/bash
# tests120.sh (lab2, rel61 tree = 7ebbc905 + dr120 test fix): what dr120 ran.
# eng.test / engrace.test: go test -c (-race) of hs2-src/engine; run from the engine dir.
set -u
D=/opt/hs2lab/dr120; E=$D/src/hs2-src/engine; cd $E
for t in TestRescueRecDualOneDeadNoRescue TestMConnRescueCappedRaw; do
  $D/eng.test -test.run "^$t\$" -test.count 30 -test.v > $D/p30_$t.txt 2>&1
  $D/engrace.test -test.run "^$t\$" -test.count 10 -test.v > $D/r10_$t.txt 2>&1
done
# CappedRaw: production DEADLINE_FIX vs the test binary's default (off); traced
HS2_MCONN_DEADLINE_FIX=1 $D/eng.test -test.run '^TestMConnRescueCappedRaw$' -test.count 60 -test.v > $D/fix60_CappedRaw.txt 2>&1
HS2_MCONN_TRACE=1 $D/engT.test -test.run '^TestMConnRescueCappedRaw$' -test.count 60 -test.v > $D/trace60_CappedRaw.txt 2>&1  # engT: + dumpMTrace cleanup
# DualOneDead / every newRRPair setup under contention (6 procs) and with the production env table (Both+Edge keys)
for i in 1 2 3 4 5 6; do HS2_MCONN_TRACE=1 $D/engT2.test -test.run TestRescueRec -test.count 25 -test.v > $D/stressRR_$i.txt 2>&1 & done; wait
for i in 1 2 3 4 5 6; do env $(cat $D/prodenv.txt) HS2_MCONN_TRACE=1 $D/engT2.test -test.run TestRescueRec -test.count 25 -test.v > $D/stressRRprod_$i.txt 2>&1 & done; wait
# fixed test
$D/engF.test -test.run '^TestMConnRescueCappedRaw$' -test.count 30 -test.v > $D/F30.txt 2>&1
$D/engFrace.test -test.run '^TestMConnRescueCappedRaw$' -test.count 10 -test.v > $D/Frace10.txt 2>&1
$D/engXrace.test -test.run '^TestMConnRescueCappedRaw$' -test.count 40 -test.v > $D/Xrace40.txt 2>&1  # X: fix forced off (failure path)

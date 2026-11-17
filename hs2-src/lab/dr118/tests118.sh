#!/bin/bash
# tests118.sh: after the A/B queue (ALLDONE.jobs118a.txt): vet, go test ./..., -race ./engine on the dr118 tree;
# then the two rescue tests that failed in dr116 (test2/race2) -count=10 on dr118 and on the pre-dr116 base (7ebbc905).
set -u
D=/opt/hs2lab/dr118; S=$D/src2/hs2-src; B=$D/base/hs2-src
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod
until [ -e $D/res/ALLDONE.jobs118a.txt ]; do sleep 5; done
cd $S
gofmt -l engine envdef cmd > $D/gofmt.txt 2>&1; go vet ./... > $D/vet.txt 2>&1; echo VETRC=$? >> $D/vet.txt
( go test -count=1 -timeout 30m ./... > $D/test.txt 2>&1; echo RC=$? >> $D/test.txt ) &
( go test -race -count=1 -timeout 30m ./engine > $D/race.txt 2>&1; echo RC=$? >> $D/race.txt ) &
wait
R='TestRescueRecDualOneDeadNoRescue|TestMConnRescueCappedRaw'
( cd $S && go test -count=10 -run "$R" ./engine > $D/flake_new.txt 2>&1; echo RC=$? >> $D/flake_new.txt )
( cd $B && go test -count=10 -run "$R" ./engine > $D/flake_base.txt 2>&1; echo RC=$? >> $D/flake_base.txt )
echo DONE > $D/TESTSDONE

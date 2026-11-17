#!/bin/bash
# tests67b.sh (rel67b, lab1)
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
O=/opt/hs2lab/r67b/res/tests; mkdir -p $O; cd /opt/hs2lab/r67b/src/hs2-src
date -u +%T > $O/start
{ echo "gofmt:"; gofmt -l . ; echo "vet:"; go vet ./... ; echo "vet rc=$?"; (cd third_party/smux && gofmt -l . && go vet ./... ; echo "smux vet rc=$?"); } > $O/fmtvet.txt 2>&1
( go test -v -run 'RescueBigBudget' ./engine > $O/budget.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/budget.txt )
go test ./... -timeout 30m > $O/full.txt 2>&1 & P1=$!
go test -race ./engine -timeout 40m > $O/race.txt 2>&1 & P2=$!
( cd third_party/smux; go test ./... -count=1 > $O/smux.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/smux.txt ) & P3=$!
( go test -race -run 'Rescue|RS169|Close|SlowTail|Trunc|Download|Keep|BigRing|MConnTable' -count=3 ./engine -timeout 40m > $O/racefilt.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/racefilt.txt ) & P4=$!
( go test -race -run TestMConnCloseRightAfterTail -count=20 -v ./engine -timeout 30m > $O/closetail20.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/closetail20.txt ) & P5=$!
wait $P1; echo "rc=$? $(date -u +%T)" >> $O/full.txt; wait $P2; echo "rc=$? $(date -u +%T)" >> $O/race.txt; wait $P3 $P4 $P5
echo DONE > $O/DONE

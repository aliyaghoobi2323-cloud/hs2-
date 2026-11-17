#!/bin/bash
# tests67.sh (rel67 = rel66b + fx171 + fx170 + rs169, lab2, under labslot --all so it never overlaps the soak)
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
O=/opt/hs2lab/r67/res/tests; mkdir -p $O; cd /opt/hs2lab/r67/src/hs2-src
date -u +%T > $O/start
{ echo "gofmt:"; gofmt -l . ; echo "vet:"; go vet ./... ; echo "vet rc=$?"; (cd third_party/smux && gofmt -l . && go vet ./... ; echo "smux vet rc=$?"); } > $O/fmtvet.txt 2>&1
go test ./... -timeout 30m > $O/full.txt 2>&1 & P1=$!
go test -race ./engine -timeout 40m > $O/race.txt 2>&1 & P2=$!
( cd third_party/smux; go test ./... -count=1 > $O/smux.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/smux.txt
  go test -race -run "AutoWin|UPD|Stale|Deadline" -count=3 ./... > $O/smuxrace.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/smuxrace.txt ) & P3=$!
wait $P1; echo "rc=$? $(date -u +%T)" >> $O/full.txt; wait $P2; echo "rc=$? $(date -u +%T)" >> $O/race.txt; wait $P3
( go test ./engine -run "CloseRightAfterTail|CloseSlowTail" -count=5 -v -timeout 10m > $O/closetail.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/closetail.txt ) & P4=$!
go test -race -run "Rescue|RS169|Close|SlowTail|Trunc|Download" -count=3 ./engine -timeout 30m > $O/racefilt.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/racefilt.txt
wait $P4
echo DONE > $O/DONE

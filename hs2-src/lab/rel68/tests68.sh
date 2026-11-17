#!/bin/bash
# tests68.sh (rel68, lab1): run under /opt/hs2lab/labslot.sh --all
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
O=/opt/hs2lab/r68/res/tests; mkdir -p $O; cd /opt/hs2lab/r68/src/hs2-src
date -u +%T > $O/start
{ echo "gofmt:"; gofmt -l . ; echo "vet:"; go vet ./... ; echo "vet rc=$?"; (cd third_party/smux && gofmt -l . && go vet ./... ; echo "smux vet rc=$?"); } > $O/fmtvet.txt 2>&1
go test ./... -timeout 40m > $O/full.txt 2>&1 & P1=$!
go test -race ./engine -timeout 45m > $O/race.txt 2>&1 & P2=$!
( cd third_party/smux; go test ./... -count=1 > $O/smux.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/smux.txt ) & P3=$!
( go test -race -run 'HalfClose|Rescue|RS169|Pick|LMFast|Steer|Spare|Hyst|Ramp|Deadband|CallStuck|Stripe|DlCopies|Close|SlowTail|Trunc' -count=2 ./engine -timeout 45m > $O/racefilt.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/racefilt.txt ) & P4=$!
wait $P1; echo "rc=$? $(date -u +%T)" >> $O/full.txt; wait $P2; echo "rc=$? $(date -u +%T)" >> $O/race.txt; wait $P3 $P4
echo DONE > $O/DONE

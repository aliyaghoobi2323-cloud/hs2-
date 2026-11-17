#!/bin/bash
# tests66.sh (rel66, lab2, under labslot --all so it never overlaps the soak)
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
O=/opt/hs2lab/r66/res/tests; mkdir -p $O; cd /opt/hs2lab/r66/src/hs2-src
date -u +%T > $O/start
go test ./... -timeout 30m > $O/full.txt 2>&1 & P1=$!
go test -race ./engine -timeout 40m > $O/race.txt 2>&1 & P2=$!
wait $P1; echo "rc=$? $(date -u +%T)" >> $O/full.txt; wait $P2; echo "rc=$? $(date -u +%T)" >> $O/race.txt
go test ./engine -run TestMConnCloseRightAfterTail -count=5 -v -timeout 10m > $O/closetail.txt 2>&1; echo "rc=$? $(date -u +%T)" >> $O/closetail.txt
# smux module unchanged by fn164: its tests ran on cb820b6c (res.v1, all pass)
echo DONE > $O/DONE

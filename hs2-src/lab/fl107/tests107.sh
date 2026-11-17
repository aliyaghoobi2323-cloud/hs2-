#!/bin/bash
# tests107.sh: gofmt / vet / go test ./... / -race ./engine ./cmd/hs2 on fl107/src (after the A/B queues)
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
F=/opt/hs2lab/fl107; O=$F/res/tests; mkdir -p $O; cd $F/src/hs2-src
gofmt -l . > $O/gofmt.txt 2>&1; go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
nice -n 19 env GOMAXPROCS=6 go test ./... -timeout 30m > $O/full.txt 2>&1; echo "rc=$?" >> $O/full.txt
nice -n 19 env GOMAXPROCS=6 go test -race ./engine ./cmd/hs2 -timeout 30m > $O/race.txt 2>&1; echo "rc=$?" >> $O/race.txt
echo DONE > $O/DONE

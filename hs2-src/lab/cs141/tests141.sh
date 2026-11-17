#!/bin/bash
# tests141.sh (cs141, lab2): vet, full tests, -race ./engine on 2 cores (nice) next to the A/B. Results res/tests.
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod GOMAXPROCS=2
R=/opt/hs2lab/cs141; O=$R/res/tests; mkdir -p $O; cd $R/src/hs2-src
gofmt -l . > $O/gofmt.txt 2>&1; echo "rc=$?" >> $O/gofmt.txt
go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
go test ./... -timeout 30m > $O/full.txt 2>&1; echo "rc=$?" >> $O/full.txt
go test -race ./engine -timeout 30m > $O/race.txt 2>&1; echo "rc=$?" >> $O/race.txt
echo DONE > $O/DONE

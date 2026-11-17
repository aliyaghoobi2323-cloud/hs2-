#!/bin/bash
# tests142.sh (df142, lab2): gofmt, vet, full tests, -race ./engine on 4 cores (nice) next to the soak. Results res/tests.
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod GOMAXPROCS=4
R=/opt/hs2lab/df142; O=$R/res/tests; mkdir -p $O; cd $R/src/hs2-src
gofmt -l . > $O/gofmt.txt 2>&1; echo "rc=$?" >> $O/gofmt.txt
go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
go test ./... -timeout 30m > $O/full.txt 2>&1; echo "rc=$?" >> $O/full.txt
go test -race ./engine -timeout 30m > $O/race.txt 2>&1; echo "rc=$?" >> $O/race.txt
echo DONE > $O/DONE

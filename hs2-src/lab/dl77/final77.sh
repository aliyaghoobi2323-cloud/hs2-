#!/bin/bash
# final77.sh: gofmt / vet / full go test / -race engine on a clean clone of dl77
set -u
O=/opt/hs2lab/dl77final; rm -rf $O; mkdir -p $O
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
git clone -q -b dl77 /opt/hs2lab/dl77.bundle $O/src && cd $O/src/hs2-src || exit 1
git log --oneline -1 > $O/head.txt
gofmt -l . | grep -v '^third_party/utls/' > $O/gofmt.txt; echo "gofmt files=$(wc -l < $O/gofmt.txt)" >> $O/sum.txt
go vet ./... > $O/vet.txt 2>&1; echo "vet rc=$?" >> $O/sum.txt
(cd third_party/smux && go test . -vet=off -count=1 > $O/smux.txt 2>&1; echo "smux test rc=$?" >> $O/sum.txt)
go test ./... -timeout 30m -count=1 > $O/test.txt 2>&1; echo "test rc=$?" >> $O/sum.txt
go test -race ./engine -timeout 25m -count=1 > $O/race.txt 2>&1; echo "race engine full rc=$?" >> $O/sum.txt
echo DONE >> $O/sum.txt

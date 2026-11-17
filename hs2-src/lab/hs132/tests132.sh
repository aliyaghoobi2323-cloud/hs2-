#!/bin/bash
# tests132.sh: on idle lab1 (single copy): gofmt, vet, go test ./..., then -race ./engine, release build from the clean clone
set -u
O=/opt/hs2lab/hs132; cd $O/repo/hs2-src
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
{ echo "gofmt:"; gofmt -l engine envdef cmd; echo "vet:"; go vet ./... && echo vet-ok; } > $O/test-lint.txt 2>&1
go test ./... -timeout 30m -count=1 > $O/test-all.txt 2>&1; echo "exit $?" >> $O/test-all.txt
go test -race ./engine -timeout 30m -count=1 > $O/test-race.txt 2>&1; echo "exit $?" >> $O/test-race.txt
echo TESTSDONE > $O/TESTSDONE

#!/bin/bash
# tests133.sh: on idle lab1 (single copy, clean clone of the commit): gofmt, vet, go test ./..., -race ./engine, release build
set -u
O=/opt/hs2lab/hs133; cd $O/clean/hs2-src
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
{ echo "gofmt:"; gofmt -l engine envdef cmd; echo "vet:"; go vet ./... && echo vet-ok; } > $O/test-lint.txt 2>&1
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $O/rel/hs2-linux-amd64 ./cmd/hs2 > $O/build.txt 2>&1; echo "exit $?" >> $O/build.txt
go test ./... -timeout 30m -count=1 > $O/test-all.txt 2>&1; echo "exit $?" >> $O/test-all.txt
go test -race ./engine -timeout 30m -count=1 > $O/test-race.txt 2>&1; echo "exit $?" >> $O/test-race.txt
echo TESTSDONE > $O/TESTSDONE

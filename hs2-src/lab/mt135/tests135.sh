#!/bin/bash
# tests135.sh: lab1, single copy: gofmt, vet, release build, go test ./..., -race on touched packages
set -u
O=/opt/hs2lab/mt135; cd $O/src/hs2-src
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
mkdir -p $O/rel
{ echo "gofmt:"; gofmt -l engine envdef cmd; echo "vet:"; go vet ./... && echo vet-ok; } > $O/test-lint.txt 2>&1
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $O/rel/hs2-linux-amd64 ./cmd/hs2 > $O/build.txt 2>&1; echo "exit $?" >> $O/build.txt
(cd $O/rel && sha256sum hs2-linux-amd64 > hs2-linux-amd64.sha256)
go test ./... -timeout 30m -count=1 > $O/test-all.txt 2>&1; echo "exit $?" >> $O/test-all.txt
go test -race ./engine ./cmd/hs2 -timeout 30m -count=1 > $O/test-race.txt 2>&1; echo "exit $?" >> $O/test-race.txt
echo TESTSDONE > $O/TESTSDONE

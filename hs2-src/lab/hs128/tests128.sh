#!/bin/bash
# tests128.sh: on idle lab1 after all runs: gofmt, vet, go test ./..., -race ./engine (concurrently), release build
set -u
cd /opt/hs2lab/hs128/src
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
O=/opt/hs2lab/hs128
{ echo "gofmt:"; gofmt -l engine envdef cmd; echo "vet:"; go vet ./engine ./envdef ./cmd/... && echo vet-ok; } > $O/test-lint.txt 2>&1
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $O/bin/hs2-linux-amd64 ./cmd/hs2 && sha256sum $O/bin/hs2-linux-amd64 > $O/bin/sha.txt
go test ./... -timeout 30m -count=1 > $O/test-all.txt 2>&1 &
go test -race ./engine -timeout 30m -count=1 > $O/test-race.txt 2>&1 &
wait
echo TESTSDONE > $O/TESTSDONE

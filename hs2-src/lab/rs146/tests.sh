#!/bin/bash
# rs146 tests: gofmt, vet, go test ./..., -race ./engine (niced, cores 17-19 while lab runs go on)
cd /opt/hs2lab/rs146/src
export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root
{ echo "gofmt: $(gofmt -l engine cmd envdef | grep -v third_party | wc -l) files"; go vet ./... 2>&1 | tail -5; echo "vet rc=${PIPESTATUS[0]}"; } > /opt/hs2lab/rs146/vet.txt 2>&1
go test ./... -count=1 -timeout 30m > /opt/hs2lab/rs146/test.txt 2>&1; echo "rc=$?" >> /opt/hs2lab/rs146/test.txt
go test -race ./engine -count=1 -timeout 30m > /opt/hs2lab/rs146/race.txt 2>&1; echo "rc=$?" >> /opt/hs2lab/rs146/race.txt

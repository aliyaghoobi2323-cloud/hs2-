#!/bin/bash
set -u
export HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod PATH=$PATH:/usr/local/go/bin CGO_ENABLED=0
cd /opt/hs2lab/bh95/src/hs2-src
go build -trimpath -ldflags="-s -w" -o /opt/hs2lab/bh95/bin/hs2-95 ./cmd/hs2; echo "build rc=$?"
sha256sum /opt/hs2lab/bh95/bin/hs2-95
( go test ./... -timeout 30m > /opt/hs2lab/bh95/test.log 2>&1; echo "EXIT $?" >> /opt/hs2lab/bh95/test.log ) &
( CGO_ENABLED=1 go test -race ./engine -timeout 40m > /opt/hs2lab/bh95/race.log 2>&1; echo "EXIT $?" >> /opt/hs2lab/bh95/race.log ) &
wait
echo TESTSDONE

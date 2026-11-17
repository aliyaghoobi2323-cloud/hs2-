#!/bin/bash
# rel69f-pre: gofmt, vet, build, go test ./... and -race ./engine (parallel)
export HOME=/root GOPATH=/root/go PATH=/usr/local/go/bin:$PATH GOPROXY=off GOFLAGS=-mod=mod
R=/opt/hs2lab/b245; cd $R/src/hs2-src || exit 1
log=$R/t.log; echo "$(date -u +%FT%TZ) start" > $log
echo "gofmt: $(gofmt -l . | grep -v '^lab/' | tr '\n' ' ')" >> $log
go vet ./... >> $log 2>&1; echo "vet rc=$?" >> $log
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.buildTag=rel69fpre" -o $R/hs2-linux-amd64 ./cmd/hs2; echo "build rc=$?" >> $log
$R/hs2-linux-amd64 version >> $log 2>&1; sha256sum $R/hs2-linux-amd64 >> $log
( nice go test ./... -timeout 60m > $R/test.out 2>&1; echo "$(date -u +%FT%TZ) test rc=$?" >> $log ) &
( nice go test -race ./engine -timeout 60m > $R/race.out 2>&1; echo "$(date -u +%FT%TZ) race rc=$? races $(grep -c 'WARNING: DATA RACE' $R/race.out)" >> $log ) &
wait; echo "$(date -u +%FT%TZ) done" >> $log

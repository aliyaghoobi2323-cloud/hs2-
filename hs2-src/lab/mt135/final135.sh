#!/bin/bash
# final135.sh (mt135): after tests3: final tree = clean + final patch; release build + sha256 (LF), cmd/hs2 tests + race
set -u
O=/opt/hs2lab/mt135; cd $O; rm -rf srcF; cp -a /opt/hs2lab/hs133/clean srcF; cd srcF; git apply ../mt135-final.patch || exit 1
cd hs2-src; export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root GOPATH=/root/go
{ gofmt -l engine envdef cmd; go vet ./... && echo vet-ok; } > $O/final-lint.txt 2>&1
mkdir -p $O/relF; CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $O/relF/hs2-linux-amd64 ./cmd/hs2; echo "build $?" >> $O/final-lint.txt
(cd $O/relF && sha256sum hs2-linux-amd64 > hs2-linux-amd64.sha256)
go test ./cmd/hs2 -count=1 > $O/final-cmd.txt 2>&1; echo "exit $?" >> $O/final-cmd.txt
go test -race ./cmd/hs2 -count=1 >> $O/final-cmd.txt 2>&1; echo "exit $?" >> $O/final-cmd.txt
echo FINALDONE > $O/FINALDONE

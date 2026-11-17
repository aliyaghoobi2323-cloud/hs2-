#!/bin/bash
# tests63c.sh (r63c, lab2, idle host): rel63c release build + sha256, gofmt, vet, then go test ./..., -race ./engine
# and the rc134/raw137 stress x10 in parallel (20 cores), smux module, installer sh tests. Results res/tests.
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
R=/opt/hs2lab/r63c; O=$R/res/tests; mkdir -p $O $R/rel; cd $R/src/hs2-src
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $R/rel/hs2-linux-amd64 ./cmd/hs2 > $O/build.txt 2>&1; echo "rc=$?" >> $O/build.txt
(cd $R/rel && sha256sum hs2-linux-amd64 > hs2-linux-amd64.sha256); cat $R/rel/hs2-linux-amd64.sha256 >> $O/build.txt
cp $R/rel/hs2-linux-amd64 $R/hs2R63C && touch $R/bin.ok
gofmt -l engine envdef cmd > $O/gofmt.txt 2>&1; echo "rc=$?" >> $O/gofmt.txt
go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
go test ./... -timeout 30m > $O/full.txt 2>&1 & P1=$!
go test -race ./engine -timeout 30m > $O/race.txt 2>&1 & P2=$!
HS2_RC134_STRESS=1 go test ./engine -run 'TestMConnRawSwitchChecksumStress|TestRawReaderNeverReadsPastLimit|TestSmuxWriteGiveUp' -count=10 -v -timeout 30m > $O/stress.txt 2>&1 & P3=$!
wait $P1; echo "rc=$?" >> $O/full.txt; wait $P2; echo "rc=$?" >> $O/race.txt; wait $P3; echo "rc=$?" >> $O/stress.txt
(cd third_party/smux && go test -vet=off ./... -timeout 10m) > $O/smux.txt 2>&1; echo "rc=$?" >> $O/smux.txt
for t in install/tests/*_test.sh; do echo "== $t" >> $O/sh.txt; timeout 600 bash "$t" >> $O/sh.txt 2>&1; echo "rc=$? $t" >> $O/sh.txt; done
echo DONE > $O/DONE

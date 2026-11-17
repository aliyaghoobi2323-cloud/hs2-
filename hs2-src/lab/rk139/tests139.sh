#!/bin/bash
# tests139.sh (rk139, lab2, idle host): rel62k2 tests. Results /opt/hs2lab/rk139/res/tests.
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
R=/opt/hs2lab/rk139; O=$R/res/tests; mkdir -p $O; cd $R/src/hs2-src
go build -o $R/hs2K2 ./cmd/hs2 > $O/build.txt 2>&1; echo "rc=$?" >> $O/build.txt; sha256sum $R/hs2K2 >> $O/build.txt
touch $R/bin.ok
go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
go test ./... -timeout 30m > $O/full.txt 2>&1; echo "rc=$?" >> $O/full.txt
go test -race ./engine -timeout 30m > $O/race.txt 2>&1; echo "rc=$?" >> $O/race.txt
(cd third_party/smux && go test ./... -timeout 10m) > $O/smux.txt 2>&1; echo "rc=$?" >> $O/smux.txt
HS2_RC134_STRESS=1 go test ./engine -run 'TestMConnRawSwitchChecksumStress|TestRawReaderNeverReadsPastLimit|TestSmuxWriteGiveUp' -count=10 -v -timeout 30m > $O/stress.txt 2>&1; echo "rc=$?" >> $O/stress.txt
for t in install/tests/*_test.sh; do echo "== $t" >> $O/sh.txt; timeout 600 bash "$t" >> $O/sh.txt 2>&1; echo "rc=$? $t" >> $O/sh.txt; done
echo DONE > $O/DONE

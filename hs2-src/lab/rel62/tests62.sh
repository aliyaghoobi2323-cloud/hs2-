#!/bin/bash
set -u
export PATH=$PATH:/usr/local/go/bin HOME=/root GOPATH=/root/go GOPROXY=off GOFLAGS=-mod=mod
R=/opt/hs2lab/rel62; O=$R/res/tests; while [ ! -e $R/soakbin.ok ]; do sleep 10; done; mkdir -p $O; cd $R/src/hs2-src
gofmt -l cmd core encap engine envdef fec mmsg obfs reality tlscarrier tun tune udpcarrier > $O/gofmt.txt 2>&1; go vet ./... > $O/vet.txt 2>&1; echo "rc=$?" >> $O/vet.txt
nice -n 19 env GOMAXPROCS=6 go test ./... -timeout 30m > $O/full.txt 2>&1; echo "rc=$?" >> $O/full.txt
nice -n 19 env GOMAXPROCS=6 go test -race ./engine ./cmd/hs2 -timeout 30m > $O/race.txt 2>&1; echo "rc=$?" >> $O/race.txt
ls install/tests > $O/shlist.txt 2>&1
for t in install/tests/*.sh; do [ -e "$t" ] || continue; echo "== $t" >> $O/sh.txt; timeout 600 bash "$t" >> $O/sh.txt 2>&1; echo "rc=$?" >> $O/sh.txt; done
echo DONE > $O/DONE

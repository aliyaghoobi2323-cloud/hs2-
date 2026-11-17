#!/bin/bash
# bh101 post-A/B: build bin101, vet/gofmt; tests on slot2 cores (its lock held) while 2 workers run j101m on slots 0/1
set -u
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0
cd /opt/hs2lab/bh101/src
gofmt -l . | grep -v third_party > ../gofmt.out
go vet ./... > ../vet.out 2>&1; echo "vet rc=$?" >> ../vet.out
go build -trimpath -ldflags="-s -w" -o ../bin/hs2-linux-amd64 ./cmd/hs2 > ../build.out 2>&1 || { echo BUILDFAIL >> ../build.out; exit 1; }
sha256sum ../bin/hs2-linux-amd64 >> ../build.out
cd /opt/hs2lab/bh100; rm -f stop
cat > j101m.txt <<J
f1bIR1 R1 20 bI 1101
f1bPR1 R1 20 bP 1101
f1bIX59 X59 20 bI 1101
f1bIX79 X79 20 bI 1101
f1bIX58 X58 20 bI 1101
f1bIK58 K58 20 bI 1101
J
( flock /opt/hs2lab/lab.slot2 bash -c "cd /opt/hs2lab/bh101/src; taskset -c 13-19 go test ./... -timeout 30m > ../test.out 2>&1; echo rc=\$? >> ../test.out; CGO_ENABLED=1 taskset -c 13-19 go test -race ./engine -timeout 30m > ../race.out 2>&1; echo rc=\$? >> ../race.out" ) &
sleep 2
for w in 0 1; do systemd-run --unit=bh101m$w --collect bash /opt/hs2lab/bh100/seq101.sh $w 2 /opt/hs2lab/bh100/j101m.txt; done
wait
echo TESTSDONE > /opt/hs2lab/bh101/TESTSDONE

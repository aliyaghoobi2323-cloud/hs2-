#!/bin/bash
# buildwin.sh TAG: a build window of the judge under the rig lock (no rig
# measurement runs meanwhile): fast-forward to r1-r3j-pc, gofmt, vet, build
# bins/hs2-TAG, the smux module tests and the engine tests named in RUN.
set -u
TAG=${1:?}; RUN=${RUN:-Hint|MoveBack|MoveRule|Quiet|OWD|Budget|Panel|LinkSndbuf|Moved|UDPLate|LocalTarget}
LOG=/root/r3j/buildwin-$TAG.log
/root/r1-quiet.take.sh r1-r3j-build "judge build window $TAG" > /dev/null || exit 1
t0=$(date +%s)
trap "rm -f /root/r1-quiet.lock; echo buildwin-$TAG \$(date -u -d @$t0 +%FT%TZ) \$(date -u +%FT%TZ) \$((\$(date +%s) - t0)) >> /root/r3j/buildtime.log" EXIT
{
cd /root/hs2-wt/r1-r3j && git merge -q --ff-only r1-r3j-pc && git log --oneline -1
cd hs2-src
/usr/local/go/bin/gofmt -l cmd engine tlscarrier third_party/smux | grep -v "angle_hqg_test.go\|stream_reverse.go"
/usr/local/go/bin/go vet ./engine ./tlscarrier ./cmd/... 2>&1 | tail -20
(cd third_party/smux && /usr/local/go/bin/go vet . 2>&1 | tail -5)
CGO_ENABLED=0 /usr/local/go/bin/go build -o /root/r3j/bins/hs2-$TAG ./cmd/hs2 && sha256sum /root/r3j/bins/hs2-$TAG
(cd third_party/smux && timeout 300 /usr/local/go/bin/go test -count=1 -run "Group|Coalesce|Sched" . 2>&1 | tail -15)
timeout 400 /usr/local/go/bin/go test -count=1 -run "$RUN" ./engine ./tlscarrier 2>&1 | tail -30
} > "$LOG" 2>&1

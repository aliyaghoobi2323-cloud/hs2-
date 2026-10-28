#!/bin/bash
# racewin.sh TAG: after the judge's last code change (the BULK link's OWD
# guard), under the rig lock: gofmt and vet of what it touched, -race on the
# engine tests of the OWD guard, the budget and the hint, and the cmd tests.
# Logs to /root/r3j/tests-TAG/race-final.txt.
set -u
TAG=${1:?}
O=/root/r3j/tests-$TAG; mkdir -p "$O"
G=/usr/local/go/bin/go
/root/r1-quiet.take.sh r1-r3j-build "judge race window $TAG" > /dev/null || exit 1
t0=$(date +%s)
trap "rm -f /root/r1-quiet.lock; echo racewin-$TAG \$(date -u -d @$t0 +%FT%TZ) \$(date -u +%FT%TZ) \$((\$(date +%s) - t0)) >> /root/r3j/buildtime.log" EXIT
cd /root/hs2-wt/r1-r3j/hs2-src
{
git log --oneline -1
echo "== gofmt -l engine/owd.go engine/judge3_test.go cmd/hs2/main.go"; /usr/local/go/bin/gofmt -l engine/owd.go engine/judge3_test.go cmd/hs2/main.go
echo "== go vet ./engine ./cmd/..."; $G vet ./engine ./cmd/... 2>&1 | tail -10
echo "== -race engine (OWD, Budget, Hint, Moved, Quiet)"; timeout 600 $G test -race -count=1 -run 'OWD|Budget|Hint|Moved|Quiet' ./engine 2>&1 | tail -15; echo "exit $?"
echo "== -race cmd"; timeout 300 $G test -race -count=1 ./cmd/... 2>&1 | tail -5
} > "$O/race-final.txt" 2>&1

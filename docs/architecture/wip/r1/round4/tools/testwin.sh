#!/bin/bash
# testwin.sh TAG: the R1 round 4 test window (no rig lock may be held: it
# exits if one is): gofmt and go vet; go test ./... (engine apart, -timeout
# 20m); the smux module; rigload; -race on what round 4 touched. Outputs to
# /root/r4/tests/TAG-*.txt.
set -u
TAG=${1:?}
O=/root/r4/tests; mkdir -p "$O"
[ -e /root/r1-quiet.lock ] && { echo "lock held: $(cat /root/r1-quiet.lock)"; exit 2; }
G=/usr/local/go/bin/go
cd /root/hs2-wt/r1-r4/hs2-src || exit 1
echo "$(date -u +%FT%TZ) start $(git log --oneline -1)" > "$O/$TAG-log.txt"
/usr/local/go/bin/gofmt -l . > "$O/$TAG-gofmt.txt" 2>&1
$G vet ./... > "$O/$TAG-vet.txt" 2>&1; echo "vet $?" >> "$O/$TAG-log.txt"
(cd third_party/smux && $G vet ./... > "$O/$TAG-vet-smux.txt" 2>&1; echo "vet smux $?" >> "$O/$TAG-log.txt")
$G test -count=1 $($G list ./... | grep -v "/engine$") > "$O/$TAG-test-rest.txt" 2>&1; echo "test rest $?" >> "$O/$TAG-log.txt"
$G test -count=1 -timeout 20m ./engine > "$O/$TAG-test-engine.txt" 2>&1; echo "test engine $?" >> "$O/$TAG-log.txt"
(cd third_party/smux && $G test -count=1 ./... > "$O/$TAG-test-smux.txt" 2>&1; echo "test smux $?" >> "$O/$TAG-log.txt")
(cd /root/hs2-wt/r1-r4/docs/architecture/wip/rig-scale/rigload && $G test -count=1 . > "$O/$TAG-test-rigload.txt" 2>&1; echo "test rigload $?" >> "$O/$TAG-log.txt")
$G test -race -count=1 -timeout 20m -run "XShare|Place|Cap|HoldFor|TwoWay|RT|Sched|Hint|Move|Quiet|Group|Budget|OWD" ./engine > "$O/$TAG-race-engine.txt" 2>&1; echo "race engine $?" >> "$O/$TAG-log.txt"
(cd third_party/smux && $G test -race -count=1 -run "Group|Sched|Coalesce|Urgent" . > "$O/$TAG-race-smux.txt" 2>&1; echo "race smux $?" >> "$O/$TAG-log.txt")
$G test -race -count=1 ./tlscarrier ./cmd/... > "$O/$TAG-race-cmd.txt" 2>&1; echo "race cmd $?" >> "$O/$TAG-log.txt"
echo "$(date -u +%FT%TZ) end" >> "$O/$TAG-log.txt"

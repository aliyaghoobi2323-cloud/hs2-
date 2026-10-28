#!/bin/bash
# testwin.sh TAG: the judge's test window under the rig lock (no rig
# measurement runs meanwhile; the next judge block waits for it): the
# branch head r1-r3j-pc, gofmt, vet, the final binary bins/hs2-TAG, every
# test of the module and of smux's, -race on what the judge touched, and
# the fuzz target of the new peer-data parser (FUZZ s). Logs to
# /root/r3j/tests-TAG/.
set -u
TAG=${1:?}; FUZZ=${FUZZ:-60}
O=/root/r3j/tests-$TAG; mkdir -p "$O"
G=/usr/local/go/bin/go
/root/r1-quiet.take.sh r1-r3j-build "judge test window $TAG" > /dev/null || exit 1
t0=$(date +%s)
trap "rm -f /root/r1-quiet.lock; echo testwin-$TAG \$(date -u -d @$t0 +%FT%TZ) \$(date -u +%FT%TZ) \$((\$(date +%s) - t0)) >> /root/r3j/buildtime.log" EXIT
cd /root/hs2-wt/r1-r3j && git merge -q --ff-only r1-r3j-pc
git log --oneline -1 > "$O/head.txt"
cd hs2-src
{
echo "== gofmt -l"; /usr/local/go/bin/gofmt -l cmd engine tlscarrier third_party/smux
echo "== go vet ./..."; $G vet ./... 2>&1 | tail -30
echo "== smux go vet"; (cd third_party/smux && $G vet . 2>&1 | tail -10)
} > "$O/vet.txt" 2>&1
CGO_ENABLED=0 $G build -o /root/r3j/bins/hs2-$TAG ./cmd/hs2 && sha256sum /root/r3j/bins/hs2-$TAG > "$O/sha256.txt"
timeout 900 $G test -count=1 ./... > "$O/test-all.txt" 2>&1; echo "exit $?" >> "$O/test-all.txt"
(cd third_party/smux && timeout 300 $G test -count=1 ./... > "$O/test-smux.txt" 2>&1; echo "exit $?" >> "$O/test-smux.txt")
timeout 600 $G test -race -count=1 -run 'Hint|MoveBack|MoveRule|Quiet|OWD|Budget|Panel|LinkSndbuf|Moved|UDPLate|LocalTarget|Group|Coalesce' ./engine > "$O/race-engine.txt" 2>&1; echo "exit $?" >> "$O/race-engine.txt"
timeout 300 $G test -race -count=1 ./tlscarrier ./cmd/... > "$O/race-tls-cmd.txt" 2>&1; echo "exit $?" >> "$O/race-tls-cmd.txt"
(cd third_party/smux && timeout 300 $G test -race -count=1 -run 'Group|Coalesce|Sched|Urgent' . > "$O/race-smux.txt" 2>&1; echo "exit $?" >> "$O/race-smux.txt")
timeout $((FUZZ + 120)) $G test -run '^$' -fuzz '^FuzzHintDecode$' -fuzztime ${FUZZ}s ./engine > "$O/fuzz-hint.txt" 2>&1; echo "exit $?" >> "$O/fuzz-hint.txt"
echo "done $(date -u +%FT%TZ)" >> "$O/head.txt"

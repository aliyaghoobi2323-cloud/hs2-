#!/bin/bash
# tests.sh: after the DPI post-processing (next.log "dpi post done"), when
# no quiet lock is held and no go build/test runs: vet, gofmt, the whole
# test suite, -race on what γ1 touched, and a short fuzz of the kindOWD
# decoder, on the r1-r3a tree; outputs in /root/r3a/tests.
set -u
LOG=/root/r3a/next.log
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
until grep -q "dpi post done" $LOG 2>/dev/null; do sleep 20; done
until [ ! -e /root/r1-quiet.lock ] && ! busy; do sleep 2; done
echo "$(date -u +%FT%TZ) tests start" >> $LOG
T=/root/r3a/tests; mkdir -p $T
cd /root/hs2-wt/r1-r3a/hs2-src
git log --oneline -1 > $T/commit.txt
G=/usr/local/go/bin/go
$G vet ./... > $T/vet.txt 2>&1; echo "exit $?" >> $T/vet.txt
(cd third_party/smux && $G vet . > $T/vet-smux.txt 2>&1; echo "exit $?" >> $T/vet-smux.txt)
/usr/local/go/bin/gofmt -l . > $T/gofmt.txt 2>&1
nice $G test -count=1 -timeout 30m ./... > $T/test-all.txt 2>&1; echo "exit $?" >> $T/test-all.txt
echo "$(date -u +%FT%TZ) go test ./... done" >> $LOG
(cd third_party/smux && nice $G test -race -count=1 ./... > $T/race-smux.txt 2>&1; echo "exit $?" >> $T/race-smux.txt)
nice $G test -race -count=1 -timeout 20m ./engine -run 'OWD|Alpha|Quiet|Group|Sched|SendCap|Move|Restamp|Coalesce' > $T/race-engine.txt 2>&1; echo "exit $?" >> $T/race-engine.txt
nice $G test -race -count=1 ./tlscarrier ./cmd/... > $T/race-other.txt 2>&1; echo "exit $?" >> $T/race-other.txt
nice $G test -count=1 -run '^$' -fuzz FuzzOWDDecode -fuzztime 60s ./engine > $T/fuzz-owd.txt 2>&1; echo "exit $?" >> $T/fuzz-owd.txt
echo "$(date -u +%FT%TZ) tests done" >> $LOG

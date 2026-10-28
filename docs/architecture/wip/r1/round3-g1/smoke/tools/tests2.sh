#!/bin/bash
# tests2.sh: the branch's tests in short chunks, each started only while no
# quiet lock is held and no other go build/test runs (other agents' blocks
# start without waiting for a running test; a chunk of a minute or two
# keeps any overlap short). Outputs in /root/r3a/tests.
set -u
LOG=/root/r3a/next.log
T=/root/r3a/tests; mkdir -p $T
G=/usr/local/go/bin/go
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
gate() { until [ ! -e /root/r1-quiet.lock ] && ! busy; do sleep 2; done; }
cd /root/hs2-wt/r1-r3a/hs2-src
git log --oneline -1 > $T/commit.txt
echo "$(date -u +%FT%TZ) chunked tests start" >> $LOG
gate; $G test -count=1 -list '.*' ./engine 2>/dev/null | grep -E '^(Test|Fuzz)' > $T/engine-tests.txt
n=$(wc -l < $T/engine-tests.txt)
: > $T/test-engine.txt
i=0
while [ $i -lt $n ]; do
  names=$(sed -n "$((i + 1)),$((i + 25))p" $T/engine-tests.txt | paste -sd'|')
  gate
  echo "== chunk $i ($(date -u +%T))" >> $T/test-engine.txt
  nice $G test -count=1 -timeout 10m -run "^($names)\$" ./engine >> $T/test-engine.txt 2>&1
  echo "exit $?" >> $T/test-engine.txt
  i=$((i + 25))
done
echo "$(date -u +%FT%TZ) engine chunks done" >> $LOG
for pkg in $($G list ./... | grep -v '/engine$'); do
  gate
  nice $G test -count=1 -timeout 10m $pkg >> $T/test-other.txt 2>&1
done
echo "exit $?" >> $T/test-other.txt
gate; (cd third_party/smux && nice $G test -race -count=1 ./... > $T/race-smux.txt 2>&1; echo "exit $?" >> $T/race-smux.txt)
gate; nice $G test -race -count=1 -timeout 10m ./engine -run 'OWD|AlphaOwn|AlphaProbe|AlphaCap|AlphaLoop|AlphaStart|QuietCounts|Restamp|MoveRule|SendCap|Coalesce' > $T/race-engine.txt 2>&1; echo "exit $?" >> $T/race-engine.txt
gate; nice $G test -race -count=1 ./tlscarrier ./cmd/... > $T/race-other.txt 2>&1; echo "exit $?" >> $T/race-other.txt
gate; nice $G test -count=1 -run '^$' -fuzz FuzzOWDDecode -fuzztime 60s ./engine > $T/fuzz-owd.txt 2>&1; echo "exit $?" >> $T/fuzz-owd.txt
echo "$(date -u +%FT%TZ) tests done" >> $LOG

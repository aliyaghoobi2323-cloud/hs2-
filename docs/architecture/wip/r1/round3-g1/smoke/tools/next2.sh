#!/bin/bash
# next2.sh: when the quiet lock is free (and no go build/test runs), the
# quick unit tests (they must pass), build hs2-g1e from the r1-r3a work
# tree, commit, then blocks F2, F3 and F4 in turn (each takes the lock
# itself; 200 s between my blocks).
set -u
LOG=/root/r3a/next.log
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
until [ ! -e /root/r1-quiet.lock ] && ! busy; do sleep 1; done
log "lock free: quick tests, then build g1e"
cd /root/hs2-wt/r1-r3a/hs2-src || exit 1
timeout 300 nice /usr/local/go/bin/go test -count=1 -run "AlphaOwn|AlphaProbe|OWD|TestAlphaCap|TestAlphaLoop|TestAlphaStart|QuietCounts|Restamp|MoveRule|SendCap" -v ./engine > /root/r3a/quicktest-g1e.txt 2>&1
e=$?; log "quick engine tests exit $e"
[ $e -eq 0 ] || exit 1
(cd third_party/smux && timeout 300 nice /usr/local/go/bin/go test -count=1 -run "Coalesce|Stamp|Sched" . >> /root/r3a/quicktest-g1e.txt 2>&1) || { log "smux tests failed"; exit 1; }
if ! CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r3a/bins/hs2-g1e ./cmd/hs2 >> "$LOG" 2>&1; then log "build failed"; exit 1; fi
sha256sum /root/r3a/bins/hs2-g1e >> /root/r3a/bins-sha256.txt
log "built g1e $(sha256sum /root/r3a/bins/hs2-g1e | cut -c1-16)"
cd /root/hs2-wt/r1-r3a
git add hs2-src
git commit -q -F /root/r3a/commit-g1e.txt && log "committed $(git log --oneline -1 | cut -c1-8)"
cd /root/r3a
tools/block2.sh F2 plans/F2.txt 2 plans/F2z.txt 1
log "F2 done"
sleep 200
tools/block2.sh F3 plans/F3.txt 2 plans/F3z.txt 1
log "F3 done"
sleep 200
tools/block2.sh F4 plans/F4.txt 2 plans/F4z.txt 1
log "F4 done"

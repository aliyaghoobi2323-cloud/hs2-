#!/bin/bash
# the β1 smoke block: waits for the lock to be free and the host quiet, holds
# /root/r1-quiet.lock, runs the plan (REPS=2, ABBA), at most 30 min
set -u
O=${1:?out}
RS=/root/hs2-wt/r1-r2a/docs/architecture/wip/rig-scale
LOCK=/root/r1-quiet.lock
mkdir -p "$O"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
w=0
while { [ -e "$LOCK" ] || busy; } && [ $w -lt 3600 ]; do sleep 20; w=$((w + 20)); done
if [ -e "$LOCK" ]; then log "lock still held after ${w}s: giving up"; exit 1; fi
touch "$LOCK"
trap "rm -f $LOCK; log lock released" EXIT
trap "exit 130" INT TERM
log "lock taken after ${w}s"
uptime >> "$O/host.txt"
REPS=1 RIG_ID=b1 BINS=/root/r2a/smoke/bins3.txt timeout 200 "$RS/run.sh" /root/r2a/smoke/plan4x.txt "$O-x"
log "run.sh exit $?"

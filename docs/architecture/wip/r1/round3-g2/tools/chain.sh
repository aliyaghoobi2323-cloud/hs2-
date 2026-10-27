#!/bin/bash
# chain.sh BUILD BLOCK...: γ2's chain (round 3). BUILD: a label (g2c) whose
# binary is built from the worktree once no quiet lock is held (the unit
# tests of the budget first, the log in /root/r3b/chain.log); then each
# BLOCK (NAME:PLAN:REPS) through block.sh, which waits for the lock and
# keeps 3 minutes after this candidate's block before.
set -u
L=/root/r3b/chain.log
B=$1; shift
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain $$ start: build $B, blocks $*"
if [ "$B" != - ]; then
  while [ -e /root/r1-quiet.lock ]; do sleep 20; done
  cd /root/hs2-wt/r1-r3b/hs2-src
  nice /usr/local/go/bin/go vet ./engine ./cmd/hs2 >> $L 2>&1
  nice /usr/local/go/bin/go test -count=1 -run "Budget|OWD|QuietCount|QuietDefaults|SendCap|ShapedConn" ./engine >> $L 2>&1
  log "unit tests exit $?"
  CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r3b/bins/hs2-$B ./cmd/hs2 && sha256sum /root/r3b/bins/hs2-$B >> /root/r3b/bins-sha256.txt && log "built $B ($(git -C /root/hs2-wt/r1-r3b rev-parse --short HEAD), worktree $(git -C /root/hs2-wt/r1-r3b status --short hs2-src | wc -l) changed)"
fi
for b in "$@"; do
  IFS=: read -r n p r <<< "$b"
  TRACE_SS=1 LIMIT=1440 /root/r3b/tools/block.sh "$n" "/root/r3b/plans/$p" "$r" >> /root/r3b/$n.nohup 2>&1
  log "block $n exit $?"
done
log "chain $$ done"

#!/bin/bash
# run-baseline.sh: the R1 baseline (task 4) on the exit server, in one go.
#  1. waits for a quiet host: no go test, *.test or go build (polled every
#     2 min, up to 30 min), then holds /root/r1-quiet.lock until it ends
#  2. field: three S5 runs on the live tunnel (field/field.sh, at most 15 min)
#  3. rig, BIN=old only: plans/rigA.txt and plans/rigB.txt side by side
#     (RIG_ID a and b; every cell at <= 100 Mbit/s), then plans/rigC.txt (the
#     1 Gbit/s cells) alone, all inside 60 min of wall time
# EDGE (ssh target of the edge over the tunnel) and PEER (the exit hs0
# address) come from the environment; nothing here names a server.
set -u
B=$(cd "$(dirname "$0")" && pwd)
RS=$(cd "$B/../../rig-scale" && pwd)
O=${O:-/root/rig-out/r1-baseline}
LOCK=/root/r1-quiet.lock
EDGE=${EDGE:?}; PEER=${PEER:?}
mkdir -p "$O"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
busy() { pgrep -f -- 'go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test' > /dev/null; }
hostsnap() {
  echo "== $1 $(date -Is)"
  uptime
  top -bn2 -d0.5 -w 200 | awk '/^top -/{f++} f==2' | sed -n '1,5p;7,14p' | cut -c1-120
}

# the labeled session (task 3, step 3b) uses the same tunnel: let it finish
while [ -n "${WAIT_FOR:-}" ] && ! grep -q '^done' "$WAIT_FOR" 2>/dev/null; do sleep 10; done
w=0
while busy && [ $w -lt 1800 ]; do
  log "host busy: $(pgrep -af -- 'go test|go build|\.test -test\.' | cut -c1-100 | paste -sd'|')"
  sleep 120; w=$((w + 120))
done
busy && log "still busy after 30 min: taking the lock anyway, the running work will not be counted as quiet"
touch "$LOCK"
trap 'rm -f "$LOCK"; log "lock released"' EXIT
trap 'exit 130' INT TERM
log "lock taken after ${w}s"
hostsnap start > "$O/host.txt"

# field
log "field start"
EDGE=$EDGE PEER=$PEER timeout 15m "$B/field/field.sh" > "$O/field.log" 2>&1
log "field done (status $?)"
hostsnap after-field >> "$O/host.txt"

# rig: 60 min from here
t0=$(date +%s); dl=$((t0 + 3600))
log "rig start"
REPS=3 RIG_ID=a timeout -s TERM $((3600 - 420))s "$RS/run.sh" "$B/plans/rigA.txt" "$O/A" > "$O/A.out" 2>&1 &
pa=$!
sleep 15
REPS=3 RIG_ID=b timeout -s TERM $((3600 - 435))s "$RS/run.sh" "$B/plans/rigB.txt" "$O/B" > "$O/B.out" 2>&1 &
pb=$!
wait $pa $pb
left=$((dl - $(date +%s)))
log "rigs A and B done, ${left}s left"
hostsnap after-AB >> "$O/host.txt"
if [ $left -gt 120 ]; then
  REPS=3 RIG_ID=c timeout -s TERM ${left}s "$RS/run.sh" "$B/plans/rigC.txt" "$O/C" > "$O/C.out" 2>&1
  log "rig C done"
fi
hostsnap end >> "$O/host.txt"
log "done"

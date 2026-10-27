#!/bin/bash
# block.sh: the rig block of R1 round 2, step 1 (diagnosis; lab only), at
# most LIMIT s (3540) of rig time in one block. Waits for a quiet host (no
# go build/test), holds /root/r1-quiet.lock for the block (trap), runs a
# smoke test on two rigs and waits for $O/GO (or $O/ABORT) before the
# phases: two rigs side by side (cells <= 100 Mbit/s), then the 1000 Mbit/s
# cells alone. Schedules and BINS: gen_sched.py.
set -u
RS=/root/hs2-wt/r1-diag/docs/architecture/wip/rig-scale
S=${S:-/root/r1-diag/sched}
O=${O:-/root/r1-diag/out}
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-3540}
mkdir -p "$O"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
busy() { pgrep -f -- 'go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test' > /dev/null; }
hostsnap() {
  echo "== $1 $(date -Is)"
  uptime
  top -bn2 -d0.5 -w 200 | awk '/^top -/{f++} f==2' | sed -n '1,5p;7,14p' | cut -c1-120
}
w=0
while busy && [ $w -lt 1800 ]; do
  log "host busy: $(pgrep -af -- 'go test|go build|\.test -test\.' | cut -c1-100 | paste -sd'|')"
  sleep 15; w=$((w + 15))
done
busy && log "still busy after 30 min: taking the lock anyway"
touch "$LOCK"
trap 'rm -f "$LOCK"; log "lock released"' EXIT
trap 'exit 130' INT TERM
log "lock taken after ${w}s"
hostsnap start > "$O/host.txt"
t0=$(date +%s); dl=$((t0 + LIMIT))
left() { echo $((dl - $(date +%s))); }

# one phase on one rig: run.sh with its own schedule
phase() { # NAME RIGID ENV...
  local name=$1 id=$2; shift 2
  env "$@" SCHED="$S/$name.tsv" RIG_ID=$id BINS="$S/bins.txt" timeout -s TERM "$(left)"s \
    "$RS/run.sh" "$S/$name.tsv" "$O/$name" > "$O/$name.out" 2>&1
  log "$name done ($(grep -c '"ok": true' "$O/$name/res.jsonl" 2>/dev/null) ok, $(grep -c '"ok": false' "$O/$name/res.jsonl" 2>/dev/null) failed), $(left)s left"
}
FULL="CAP=1 TRACE=1 TCS=1"

log "smoke"
phase smoke1 d1 $FULL &
sleep 3
phase smoke2 d2 $FULL &
wait
log "smoke done; waiting for GO"
while [ ! -e "$O/GO" ] && [ ! -e "$O/ABORT" ] && [ "$(left)" -gt 600 ]; do sleep 2; done
[ -e "$O/GO" ] || { log "no GO: stop"; exit 1; }
log "GO, $(left)s left"

( phase q12-fq d1 $FULL; phase q3-1 d1 TRACE=1 ) &
p1=$!
sleep 8
( phase q12-pf d2 $FULL; phase q3-2 d2 TRACE=1 ) &
p2=$!
wait $p1 $p2
hostsnap after-par >> "$O/host.txt"
[ "$(left)" -gt 120 ] && phase q4-solo d3 TRACE=1
hostsnap end >> "$O/host.txt"
log "done after $(( $(date +%s) - t0 ))s of rig time"

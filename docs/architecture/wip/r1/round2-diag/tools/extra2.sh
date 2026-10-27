#!/bin/bash
# extra2.sh: the last part of the same rig block (round 2, step 1). The route
# "cwnd lock" of extra.sh did not hold either (a new socket takes the clamp
# only from a cached tcp_metrics entry, and the links' sockets had none: the
# exit's cwnd reached 88 under a lock of 36). So the window clamp once more,
# now with the receive buffer locked first (labtrace.go), from a fresh
# binary (hs2-diag-a2), on the six Q1/Q2 cells, two repeats, until the
# block's deadline (LOCK0 + LIMIT).
set -u
RS=/root/hs2-wt/r1-diag/docs/architecture/wip/rig-scale
S=/root/r1-diag/sched
O=/root/r1-diag/out
LOCK=/root/r1-quiet.lock
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
t0=$(date -d "$(awk '/lock taken/{print $1; exit}' "$LOG")" +%s)
dl=$((t0 + ${LIMIT:-3540}))
left() { echo $((dl - $(date +%s))); }
until grep -q "extra done after" "$LOG" && ! pgrep -f r1-diag-work/extra.sh > /dev/null; do sleep 1; done
touch "$LOCK"
trap 'rm -f "$LOCK"; log "lock released (extra2)"' EXIT
trap 'exit 130' INT TERM
log "extra2: lock held again, $(left)s left in the block; building hs2-diag-a2"
(cd /root/hs2-wt/r1-diag/hs2-src && CGO_ENABLED=0 nice -n 10 /usr/local/go/bin/go build -o /root/r1-bins/hs2-diag-a2 ./cmd/hs2) >> "$O/extra2.build" 2>&1 || { log "extra2: build failed"; exit 1; }
log "extra2: built, $(left)s left"
[ "$(left)" -gt 150 ] || exit 0
phase() {
  local name=$1 id=$2
  env CAP=1 TRACE=1 TCS=1 SCHED="$S/$name.tsv" RIG_ID=$id BINS="$S/bins-x2.txt" timeout -s TERM "$(($(left) - 5))"s \
    "$RS/run.sh" "$S/$name.tsv" "$O/$name" > "$O/$name.out" 2>&1
  log "$name done ($(grep -c '"ok": true' "$O/$name/res.jsonl" 2>/dev/null) ok), $(left)s left"
}
phase x2-fq d1 &
sleep 8
phase x2-pf d2 &
wait
log "extra2 done after $(( $(date +%s) - t0 ))s of rig time"

#!/bin/bash
# extra.sh: the rest of the same rig block (round 2, step 1). Waits for
# block.sh to finish its phases, holds /root/r1-quiet.lock again at once (the
# same block: it ends no later than LOCK0 + LIMIT, the block's own
# deadline), runs the x-fq and x-pf schedules (gen_extra.py) on two rigs
# with run_x.sh (run.sh + CWNDLOCK), and releases the lock.
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
until grep -q "done after" "$LOG" && ! pgrep -f r1-diag-work/block.sh > /dev/null; do sleep 1; done
touch "$LOCK"
trap 'rm -f "$LOCK"; log "lock released (extra)"' EXIT
trap 'exit 130' INT TERM
log "extra: lock held again, $(left)s left in the block"
[ "$(left)" -gt 120 ] || exit 0
phase() {
  local name=$1 id=$2
  env CAP=1 TRACE=1 TCS=1 SCHED="$S/$name.tsv" RIG_ID=$id BINS="$S/bins-x.txt" timeout -s TERM "$(left)"s \
    "$RS/run_x.sh" "$S/$name.tsv" "$O/$name" > "$O/$name.out" 2>&1
  log "$name done ($(grep -c '"ok": true' "$O/$name/res.jsonl" 2>/dev/null) ok), $(left)s left"
}
phase x-fq d1 &
sleep 8
phase x-pf d2 &
wait
log "extra done after $(( $(date +%s) - t0 ))s of rig time"

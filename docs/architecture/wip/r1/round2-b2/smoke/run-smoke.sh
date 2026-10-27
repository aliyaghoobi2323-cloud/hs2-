#!/bin/bash
# β2 smoke on the big rig: holds /root/r1-quiet.lock for the whole block
set -u
LOCK=/root/r1-quiet.lock
OUT=${1:?usage: run-smoke.sh OUT [PLAN]}
PLAN=${2:-/root/r2b-smoke/plan.txt}
if [ -e "$LOCK" ]; then echo "lock held: $(cat $LOCK)"; exit 1; fi
echo "r1-r2b (beta2) smoke, $(date -Is), pid $$" > "$LOCK"
trap 'rm -f "$LOCK"' EXIT INT TERM
echo "start $(date -Is)" >> /root/r2b-smoke/orch.log
cd /root/hs2-wt/r1-r2b/docs/architecture/wip/rig-scale
BINS=/root/r2b-smoke/bins.txt REPS=${REPS:-2} RIG_ID=b2 ./run.sh "$PLAN" "$OUT"
echo "end $(date -Is) $OUT" >> /root/r2b-smoke/orch.log

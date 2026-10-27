#!/bin/bash
# block.sh NAME PLAN [REPS]: one locked rig block of the R1 round-2 judge, at
# most LIMIT s (1750) of rig time, one rig (RIG_ID j). Waits for the quiet lock
# to be free and for no go build/test, holds /root/r1-quiet.lock, runs the
# plan (ABBABA, REPS each; base = old = OLD_BIN, the rest from BINS), then
# reruns runs whose host was busier than 75 % or that failed, while time is
# left. Raw run files go to /root/r2j/raw-NAME (not backed up), the results
# (res.jsonl, run.log, plan, schedule, orch.log, host.txt) to /root/r2j/NAME.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-3}
RS=/root/hs2-wt/r1-r2j/docs/architecture/wip/rig-scale
O=/root/r2j/$NAME; R=/root/r2j/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1750}
BINS=${BINS:-/root/r2j/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r2j/bins/hs2-base}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
w=0
while { [ -e "$LOCK" ] || busy; } && [ $w -lt 1200 ]; do sleep 15; w=$((w + 15)); done
if [ -e "$LOCK" ]; then log "lock still held after ${w}s ($(cat $LOCK)): giving up"; exit 1; fi
echo "r1-r2j judge block $NAME, pid $$, $(date -Is)" > "$LOCK"
trap "rm -f $LOCK; log lock released" EXIT
trap "exit 130" INT TERM
log "lock taken after ${w}s (pid $$), plan $PLAN, REPS $REPS, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
t0=$(date +%s); dl=$((t0 + LIMIT))
left() { echo $((dl - $(date +%s))); }
run() { REPS=$REPS RIG_ID=j BINS=$BINS OLD_BIN=$OLD_BIN timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1; }
run
for k in 1 2; do
  n=$(python3 - "$R/res.jsonl" <<PY
import json, sys
p = sys.argv[1]
keep, out = [], []
for l in open(p).read().splitlines():
    if not l.strip():
        continue
    r = json.loads(l)
    (out if (r.get("host_busy_pct") or 0) > 75 or not r.get("ok") else keep).append(l)
if out:
    open(p[:-6] + ".dropped.jsonl", "a").write("\n".join(out) + "\n")
    open(p, "w").write("\n".join(keep) + "\n")
print(len(out))
PY
)
  log "pass $k: $n runs busy (>75%) or failed, $(left)s left"
  [ "$n" -gt 0 ] && [ "$(left)" -gt 120 ] || break
  run
done
hostsnap end >> "$O/host.txt"
for f in res.jsonl res.dropped.jsonl run.log plan.txt schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
log "done after $(( $(date +%s) - t0 ))s of rig time"

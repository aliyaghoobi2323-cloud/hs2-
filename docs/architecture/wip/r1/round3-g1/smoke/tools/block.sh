#!/bin/bash
# block.sh NAME PLAN [REPS]: one locked rig block of R1 round 3, candidate
# g1 (r1-r3a), at most LIMIT s (1500) of rig time, rig RIG_ID a. Takes the
# shared quiet lock /root/r1-quiet.lock atomically (noclobber), polling every
# 60 s while someone else holds it (never removing a lock of another), waits
# for no go build/test, runs the plan (old = OLD_BIN, the rest from BINS),
# reruns runs that failed or whose host was busier than 75 % while time is
# left, and releases the lock at once. Raw files: /root/r3a/raw-NAME (not
# backed up); results: /root/r3a/NAME.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-2}
RS=/root/r3a/rig
O=/root/r3a/$NAME; R=/root/r3a/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1500}
BINS=${BINS:-/root/r3a/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3a/bins/hs2-old}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
w=0
while :; do
  if ! busy && ( set -C; echo "r1-r3a γ1 block $NAME, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; then break; fi
  if [ $w -ge 5400 ]; then log "no lock after ${w}s ($(cat $LOCK 2>/dev/null)): giving up"; exit 1; fi
  sleep 60; w=$((w + 60))
done
trap 'rm -f "$LOCK"; log "lock released"' EXIT
trap 'exit 130' INT TERM
log "lock taken after ${w}s (pid $$), plan $PLAN, REPS $REPS, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
t0=$(date +%s); dl=$((t0 + LIMIT))
[ -x "$R/rigload" ] || cp /root/r3a/rigload "$R/rigload"
left() { echo $((dl - $(date +%s))); }
run() {
  REPS=$REPS RIG_ID=a BINS=$BINS OLD_BIN=$OLD_BIN TRACE_SS=${TRACE_SS:-0} timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
}
run
for k in 1 2; do
  n=$(python3 - "$R/res.jsonl" <<'PY'
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
  if [ "$n" -gt 0 ] && [ "$(left)" -gt 120 ]; then run; else break; fi
done
hostsnap end >> "$O/host.txt"
for f in res.jsonl res.dropped.jsonl run.log plan.txt schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
log "done after $(( $(date +%s) - t0 ))s of rig time"

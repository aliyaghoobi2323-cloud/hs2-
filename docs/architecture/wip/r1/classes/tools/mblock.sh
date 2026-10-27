#!/bin/bash
# mblock.sh NAME PLAN [PLAN2 REPS2]: one locked rig block of the classes smoke (R1 round 3,
# branch r1-mix), RIG_ID=m, at most LIMIT s (1200) of rig time. The quiet
# lock /root/r1-quiet.lock is taken atomically (set -C) when no go build/test
# runs, tried at most once every 60 s while someone else holds it (the file
# is looked at every 5 s), and only ever removed
# by this script for its own pid. The rig is a frozen copy (/root/rmix/rig)
# and rigload is prebuilt (/root/rmix/rigload): nothing is built under the
# lock. Runs: REPS (2) each binary, interleaved (ABBA); base = old = OLD_BIN,
# b2nx from BINS; PLAN2 (optional) then runs with REPS2 each, in the same
# block and the same res.jsonl. Runs that failed or whose host was busier
# than 75 % are run again while time is left. Raw run files go to
# /root/rmix/raw-NAME (never committed), the results to /root/rmix/NAME.
set -u
NAME=${1:?usage: mblock.sh NAME PLAN}; PLAN=${2:?usage: mblock.sh NAME PLAN}
PLAN2=${3:-}; REPS2=${4:-1}
REPS=${REPS:-2}
RS=/root/rmix/rig
O=/root/rmix/$NAME; R=/root/rmix/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1200}
BINS=${BINS:-/root/rmix/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r2j/bins/hs2-base}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|go vet|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; top -bn2 -d0.5 -w 200 | awk '/^top -/{f++} f==2' | sed -n '1,5p;7,14p' | cut -c1-120; }
mine() { grep -q "pid $$," "$LOCK" 2>/dev/null; }
[ -x "$R/rigload" ] || cp /root/rmix/rigload "$R/rigload"
touch "$R/rigload"
# the lock file is looked at every 5 s (a read, as the other agents' lock
# scripts do), but taking it is tried at most once every 60 s
w=0; last=-60
while :; do
  if [ ! -e "$LOCK" ] && ! busy && [ $((w - last)) -ge 60 ]; then
    last=$w
    if ( set -C; echo "r1-mix classes smoke $NAME, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; then
      break
    fi
    log "lost the lock to: $(cat "$LOCK" 2>/dev/null)"
  elif [ ! -e "$LOCK" ] && busy && [ "${nb:-0}" = 0 ]; then
    log "lock free, a go build/test runs: waiting"
  fi
  nb=0; [ ! -e "$LOCK" ] && busy && nb=1
  if [ $w -ge 3000 ]; then log "lock still held after ${w}s ($(cat $LOCK 2>/dev/null)): giving up"; exit 1; fi
  sleep 5; w=$((w + 5))
done
trap 'mine && rm -f "$LOCK"; log "lock released"' EXIT
trap 'exit 130' INT TERM
log "lock taken after ${w}s (pid $$), plan $PLAN, REPS $REPS${PLAN2:+, then $PLAN2, REPS $REPS2}, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
t0=$(date +%s); dl=$((t0 + LIMIT))
left() { echo $((dl - $(date +%s))); }
run() {
  REPS=$REPS RIG_ID=m BINS=$BINS OLD_BIN=$OLD_BIN LOAD_WAIT=60 \
    timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
  if [ -n "$PLAN2" ] && [ "$(left)" -gt 60 ]; then
    REPS=$REPS2 RIG_ID=m BINS=$BINS OLD_BIN=$OLD_BIN LOAD_WAIT=60 \
      timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN2" "$R" >> "$O/run.out" 2>&1
  fi
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
  [ "$n" -gt 0 ] && [ "$(left)" -gt 120 ] || break
  run
done
hostsnap end >> "$O/host.txt"
for f in res.jsonl res.dropped.jsonl run.log plan.txt schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
log "done after $(( $(date +%s) - t0 ))s of rig time"

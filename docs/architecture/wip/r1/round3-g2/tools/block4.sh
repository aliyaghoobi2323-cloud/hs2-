#!/bin/bash
# block4.sh NAME PLAN [REPS]: block3.sh polling each 5 s (at 20 s it still
# lost the lock for two hours: other agents take it within seconds of its
# release). block3.sh: block2.sh polling the lock each 20 s (at 60 s
# the lock passed between other agents within seconds, and γ2's blocks
# waited 50 minutes and more). block2.sh: block.sh with RS (the rig copy) and RLBIN (its
# rigload) set from outside. One locked rig block of R1 round 3, candidate γ2
# (branch r1-r3b), on the rig shared with candidate γ1: rig namespaces with
# RIG_ID b, at most LIMIT s (1440) of rig time.
#   - waits (polling each 5 s, at most WAITMAX s) while the quiet lock
#     /root/r1-quiet.lock is held by anyone or a go build/test runs, and
#     until GAP s (180) have passed since this candidate's previous block;
#   - takes the lock atomically (set -C), runs the plan (ABBABA, REPS each;
#     base = old = OLD_BIN, the rest from BINS), reruns runs whose host was
#     busier than 75 % or that failed while time is left, and removes the
#     lock right after;
#   - raw run files to /root/r3b/raw-NAME (not backed up), results
#     (res.jsonl, run.log, plan, schedule, orch.log, host.txt) to
#     /root/r3b/NAME, and the locked time to /root/r3b/locktime.log.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-3}
RS=${RS:-/root/r3b/rig}; RLBIN=${RLBIN:-/root/r3b/rigload}
O=/root/r3b/$NAME; R=/root/r3b/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1440}; GAP=${GAP:-180}; WAITMAX=${WAITMAX:-3600}
BINS=${BINS:-/root/r3b/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3b/bins/hs2-base}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
tr -d '\r' < "$PLAN" > "$O/plan.txt"; PLAN=$O/plan.txt
w=0
while :; do
  last=$(cat /root/r3b/last-block-end 2>/dev/null || echo 0)
  if [ $(( $(date +%s) - last )) -ge "$GAP" ] && ! busy && ( set -C; echo "r1-r3b γ2 block $NAME, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; then
    break
  fi
  if [ $w -ge "$WAITMAX" ]; then log "no lock after ${w}s ($(cat $LOCK 2>/dev/null)): giving up"; exit 1; fi
  sleep 5; w=$((w + 5))
done
t0=$(date +%s)
trap 'rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3b/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3b/locktime.log; log "lock released after $((t1 - t0))s"' EXIT
trap "exit 130" INT TERM
log "lock taken after ${w}s (pid $$), plan $PLAN, REPS $REPS, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
dl=$((t0 + LIMIT))
[ -x "$R/rigload" ] || cp "$RLBIN" "$R/rigload"
left() { echo $((dl - $(date +%s))); }
run() {
  REPS=$REPS RIG_ID=b BINS=$BINS OLD_BIN=$OLD_BIN timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
}
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
for f in res.jsonl res.dropped.jsonl run.log schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
log "done after $(( $(date +%s) - t0 ))s of rig time"

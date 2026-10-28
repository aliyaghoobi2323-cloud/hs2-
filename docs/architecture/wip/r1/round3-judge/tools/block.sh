#!/bin/bash
# block.sh NAME PLAN [REPS]: one locked rig block of the R1 round-3 judge
# (branch r1-r3j), rig namespaces with RIG_ID j3, at most LIMIT s (1440) of
# rig time.
#   - waits until GAP s (150) have passed since the judge's previous block,
#     then takes the shared lock /root/r1-quiet.lock through the fair FIFO
#     script /root/r1-quiet.take.sh (first come, first served);
#   - runs the plan (ABBABA-style rotation, REPS each; base = old = OLD_BIN,
#     the rest from BINS), reruns runs that failed or whose host was busier
#     than 75 % while time is left, and removes the lock right after;
#   - raw run files to /root/r3j/raw-NAME (not backed up), results
#     (res.jsonl, run.log, plan, schedule, orch.log, host.txt) to
#     /root/r3j/NAME, the locked time to /root/r3j/locktime.log.
# Nothing is built under the lock: rigload is copied in from RLBIN.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-2}
RS=${RS:-/root/r3j/rig}; RLBIN=${RLBIN:-/root/r3j/rigload}
O=/root/r3j/$NAME; R=/root/r3j/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1440}; GAP=${GAP:-150}
BINS=${BINS:-/root/r3j/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3j/bins/hs2-base}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
tr -d '\r' < "$PLAN" > "$O/plan.txt"; PLAN=$O/plan.txt
# one judge block at a time (blocks queue here), then GAP s after the previous one ended
exec 9>/root/r3j/.serial
flock 9
while last=$(cat /root/r3j/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
w0=$(date +%s)
/root/r1-quiet.take.sh r1-r3j "judge block $NAME" >> "$LOG" 2>&1 || { log "take failed"; exit 1; }
t0=$(date +%s)
trap 'rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3j/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3j/locktime.log; log "lock released after $((t1 - t0))s"' EXIT
trap "exit 130" INT TERM
log "lock taken after $((t0 - w0))s (pid $$), plan $PLAN, REPS $REPS, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
dl=$((t0 + LIMIT))
[ -x "$R/rigload" ] || cp "$RLBIN" "$R/rigload"
left() { echo $((dl - $(date +%s))); }
run() {
  REPS=$REPS RIG_ID=q BINS=$BINS OLD_BIN=$OLD_BIN TRACE_SS=${TRACE_SS:-0} timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
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
    open(p, "w").write("\n".join(keep) + ("\n" if keep else ""))
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

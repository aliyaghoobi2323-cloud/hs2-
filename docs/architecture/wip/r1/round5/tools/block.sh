#!/bin/bash
# block.sh NAME PLAN [REPS]: one locked rig block of the R1 round 5
# (branch r1-r5), rig namespaces with RIG_ID 5, at most LIMIT s (1440) of
# rig time. From round 4's block.sh, with the judge's paths:
#   - waits until GAP s (150) have passed since this agent's previous block,
#     then takes the shared lock /root/r1-quiet.lock only through the fair
#     FIFO script /root/r1-quiet.take.sh, and removes it right after;
#   - runs the plan with run.sh (memory guard: log only, the owner's T21),
#     REPS each in the ABBABA-style rotation; reruns the runs that failed or
#     whose host was busier than 75 % while time is left;
#   - raw files to /root/r5/raw-NAME (not in the repo), results to
#     /root/r4j/NAME, the locked time to /root/r5/locktime.log.
# Nothing is built under the lock: rigload is copied in from RLBIN.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-2}
RS=${RS:-/root/hs2-wt/r1-r5/docs/architecture/wip/rig-scale}; RLBIN=${RLBIN:-/root/r5/raw-bins/rigload}
O=/root/r5/$NAME; R=/root/r5/raw-$NAME
LOCK=/root/r1-quiet.lock
LIMIT=${LIMIT:-1440}; GAP=${GAP:-130}
BINS=${BINS:-/root/r5/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r4j/bins/hs2-j3g}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
hostsnap() { echo "== $1 $(date -Is)"; uptime; grep TCP: /proc/net/sockstat; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
tr -d '\r' < "$PLAN" > "$O/plan.txt"; PLAN=$O/plan.txt
exec 9>/root/r5/.serial
flock 9
while last=$(cat /root/r5/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
w0=$(date +%s)
/root/r1-quiet.take.sh r1-r5 "R1 round 5 block $NAME" >> "$LOG" 2>&1 || { log "take failed"; exit 1; }
t0=$(date +%s)
trap 'rm -f /root/r1-quiet.lock; t1=$(date +%s); echo $t1 > /root/r5/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r5/locktime.log; log "lock released after $((t1 - t0))s"' EXIT
trap "exit 130" INT TERM
log "lock taken after $((t0 - w0))s (pid $$), plan $PLAN, REPS $REPS, limit ${LIMIT}s"
hostsnap start > "$O/host.txt"
dl=$((t0 + LIMIT))
[ -x "$R/rigload" ] || cp "$RLBIN" "$R/rigload"
left() { echo $((dl - $(date +%s))); }
run() {
  REPS=$REPS RIG_ID=5 RIG_CPUS=${RIG_CPUS:-0-5} BINS=$BINS OLD_BIN=$OLD_BIN TRACE_SS=${TRACE_SS:-0} timeout -s TERM "$(left)"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
  log "run.sh status $?"
}
run
for k in 1 2; do
  n=$(python3 - "$R/res.jsonl" <<PY
import json, sys
p = sys.argv[1]
keep, out = [], []
try:
    lines = open(p).read().splitlines()
except FileNotFoundError:
    lines = []
for l in lines:
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
for g in "$R"/guard-*; do [ -d "$g" ] && cp -r "$g" "$O/"; done
grep -h "ABORT\|end:" "$O"/guard-*/guard.log >> "$LOG" 2>/dev/null
log "done after $(( $(date +%s) - t0 ))s of rig time"

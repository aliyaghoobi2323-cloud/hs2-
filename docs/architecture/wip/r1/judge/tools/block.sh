#!/bin/bash
# block.sh NAME: one rig block of the R1 judge (task 17), at most 60 min of
# rig time. Waits for a quiet host (no go build/test), holds
# /root/r1-quiet.lock for the block, runs PAR1 and PAR2 side by side
# (RIG_ID j1, j2; cells <= 100 Mbit/s), then SOLO alone (RIG_ID j3; the
# 1000 Mbit/s cells), then reruns runs whose host was busier than 75 % while
# time is left. Plans and BINS come from the environment.
set -u
NAME=${1:?}
RS=/root/hs2-wt/r1-judge/docs/architecture/wip/rig-scale
O=/root/r1-judge/$NAME
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
  sleep 30; w=$((w + 30))
done
busy && log "still busy after 30 min: taking the lock anyway"
touch "$LOCK"
trap 'rm -f "$LOCK"; log "lock released"' EXIT
trap 'exit 130' INT TERM
log "lock taken after ${w}s"
hostsnap start > "$O/host.txt"
t0=$(date +%s); dl=$((t0 + LIMIT))
left() { echo $((dl - $(date +%s))); }

# busier than 75 %: out of res.jsonl (kept in res.busy.jsonl), so run.sh reruns it
prune() {
  python3 - "$1" <<'PY'
import json, sys
p = sys.argv[1]
keep, busy = [], []
try:
    lines = open(p).read().splitlines()
except OSError:
    sys.exit(0)
for l in lines:
    if not l.strip():
        continue
    r = json.loads(l)
    (busy if (r.get("host_busy_pct") or 0) > 75 else keep).append(l)
if busy:
    open(p[:-6] + ".busy.jsonl", "a").write("\n".join(busy) + "\n")
    open(p, "w").write("\n".join(keep) + "\n")
print(len(busy))
PY
}

par() {
  local pids=() i=0 p
  for p in "$@"; do
    i=$((i + 1))
    [ -s "$p" ] || continue
    REPS=3 RIG_ID=j$i BINS=$BINS timeout -s TERM "$(left)"s "$RS/run.sh" "$p" "$O/r$i" > "$O/r$i.out" 2>&1 &
    pids+=($!)
    sleep 10
  done
  [ ${#pids[@]} -gt 0 ] && wait "${pids[@]}"
}

log "rig start, limit ${LIMIT}s"
if [ -n "${SOLO:-}" ] && [ "${SOLO_FIRST:-1}" = 1 ]; then
  REPS=3 RIG_ID=j3 BINS=$BINS timeout -s TERM "${SOLO_MAX:-$(left)}"s "$RS/run.sh" "$SOLO" "$O/r3" > "$O/r3.out" 2>&1
  log "solo done, $(left)s left"
  hostsnap after-solo >> "$O/host.txt"
fi
par "${PAR1:-}" "${PAR2:-}"
log "parallel done, $(left)s left"
hostsnap after-par >> "$O/host.txt"
if [ -n "${SOLO:-}" ] && [ "${SOLO_FIRST:-1}" != 1 ] && [ "$(left)" -gt 60 ]; then
  REPS=3 RIG_ID=j3 BINS=$BINS timeout -s TERM "$(left)"s "$RS/run.sh" "$SOLO" "$O/r3" > "$O/r3.out" 2>&1
  log "solo done, $(left)s left"
fi
# reruns: busy runs and failed runs, while time is left
for k in 1 2 3; do
  [ -f "$O/r$k/res.jsonl" ] || continue
  n=$(prune "$O/r$k/res.jsonl")
  nf=$(grep -c '"ok": false' "$O/r$k/res.jsonl")
  log "r$k: $n runs over 75 % busy, $nf failed"
done
if [ "$(left)" -gt 90 ]; then
  for k in 1 2; do
    p=PAR$k
    [ -n "${!p:-}" ] && [ -f "$O/r$k/res.jsonl" ] || continue
    REPS=3 RIG_ID=j$k BINS=$BINS timeout -s TERM "$(left)"s "$RS/run.sh" "${!p}" "$O/r$k" >> "$O/r$k.out" 2>&1 &
  done
  wait
  if [ -n "${SOLO:-}" ] && [ "$(left)" -gt 60 ]; then
    REPS=3 RIG_ID=j3 BINS=$BINS timeout -s TERM "$(left)"s "$RS/run.sh" "$SOLO" "$O/r3" >> "$O/r3.out" 2>&1
  fi
  log "reruns done, $(left)s left"
fi
hostsnap end >> "$O/host.txt"
log "done after $(( $(date +%s) - t0 ))s of rig time"

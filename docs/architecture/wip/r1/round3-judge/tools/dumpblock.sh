#!/bin/bash
# dumpblock.sh NAME PLAN [AT]: a locked diagnostic rig block of the R1
# round-3 judge: the plan runs once (REPS=1, no reruns), and AT s (22) after
# each run's edge hs2 starts, both hs2 processes get SIGQUIT with
# GOTRACEBACK=all: their goroutine dumps land in the run's edge.log and
# exit.log (the run then counts as failed: the hs2 exited). Raw files to
# /root/r3j/raw-NAME, the lock as block.sh (fair FIFO, released at once).
set -u
NAME=${1:?}; PLAN=${2:?}; AT=${3:-22}
RS=${RS:-/root/r3j/rig}; RLBIN=${RLBIN:-/root/r3j/rigload}
O=/root/r3j/$NAME; R=/root/r3j/raw-$NAME
LOCK=/root/r1-quiet.lock
BINS=${BINS:-/root/r3j/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3j/bins/hs2-base}
GAP=${GAP:-150}; LIMIT=${LIMIT:-900}
mkdir -p "$O" "$R"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
tr -d '\r' < "$PLAN" > "$O/plan.txt"; PLAN=$O/plan.txt
# one judge block at a time (blocks queue here), then GAP s after the previous one ended
exec 9>/root/r3j/.serial
flock 9
while last=$(cat /root/r3j/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-r3j "judge diagnostic $NAME" >> "$LOG" 2>&1 || exit 1
t0=$(date +%s)
WPID=
trap '[ -n "$WPID" ] && kill $WPID 2>/dev/null; rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3j/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3j/locktime.log; log "lock released after $((t1 - t0))s"' EXIT
trap "exit 130" INT TERM
log "lock taken, plan $PLAN, SIGQUIT at ${AT}s"
[ -x "$R/rigload" ] || cp "$RLBIN" "$R/rigload"
# the watcher: each edge hs2 of this rig (RIG_ID q) gets SIGQUIT AT s after it starts, with its exit
(
  declare -A seen
  while :; do
    for p in $(pgrep -f "run -c /tmp/rig-scaleq/.*/edge.json"); do
      [ -n "${seen[$p]:-}" ] && continue
      seen[$p]=1
      (
        sleep "$AT"
        d=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -o '/tmp/rig-scaleq/[^/]*' | head -1)
        x=$(pgrep -f "run -c $d/exit.json" | head -1)
        echo "$(date +%T) SIGQUIT edge $p exit ${x:-none} ($d)" >> "$LOG"
        kill -QUIT "$p" 2>/dev/null
        [ -n "$x" ] && kill -QUIT "$x" 2>/dev/null
      ) &
    done
    sleep 1
  done
) &
WPID=$!
REPS=1 RIG_ID=q BINS=$BINS OLD_BIN=$OLD_BIN GOTRACEBACK=all timeout -s TERM "$LIMIT"s "$RS/run.sh" "$PLAN" "$R" >> "$O/run.out" 2>&1
for f in res.jsonl run.log schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
log "done"

#!/bin/bash
# xblock.sh NAME PLAN OLD NEW: one locked rig block of the judge's xray runs
# (the xray tools' xrun.sh, RIG_ID q, the judge's own test keys in
# /root/r3j-xkeys), the old and new binary given by path; the lock as
# block.sh (fair FIFO, GAP s after the judge's previous block, released at
# once).
set -u
NAME=${1:?}; PLAN=${2:?}; OLD=${3:?}; NEW=${4:?}
XD=/root/hs2-wt/r1-r3j/docs/architecture/wip/r1/xray
O=/root/r3j/$NAME
LOCK=/root/r1-quiet.lock
GAP=${GAP:-150}; LIMIT=${LIMIT:-1440}
mkdir -p "$O"
LOG=$O/orch.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
exec 9>/root/r3j/.serial
flock 9
while last=$(cat /root/r3j/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-r3j "judge xray $NAME" >> "$LOG" 2>&1 || exit 1
t0=$(date +%s)
trap 'rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3j/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3j/locktime.log; log "lock released after $((t1 - t0))s"' EXIT
trap "exit 130" INT TERM
log "lock taken: plan $PLAN, old $OLD, new $NEW"
RIG_ID=q KEYS=/root/r3j-xkeys OLD_BIN=$OLD NEW_BIN=$NEW NEW_ENV= RIG=/root/r3j/rig timeout -s TERM "$LIMIT"s "$XD/xrun.sh" "$PLAN" "$O/out" >> "$O/run.out" 2>&1
log "done"

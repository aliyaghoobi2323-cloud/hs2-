#!/bin/bash
# lockrun.sh NAME WHAT LIMIT CMD...: one locked block of the R1 merge (step 5).
# Waits GAP s (150) after this agent's previous block, takes the shared rig
# lock only through the fair FIFO /root/r1-quiet.take.sh, runs CMD in a
# process group of its own for at most LIMIT s (<= 1500), releases the lock
# right after (also on failure or a signal), and logs the locked time to
# /root/rmerge/locktime.log. CMD's output: /root/rmerge/logs/NAME-<time>.txt.
set -u
NAME=${1:?}; WHAT=${2:?}; LIMIT=${3:?}; shift 3
[ "$LIMIT" -le 1500 ] || LIMIT=1500
O=/root/rmerge; mkdir -p "$O/logs"
GAP=${GAP:-150}
exec 9>"$O/.serial"
flock 9
while last=$(cat "$O/last-block-end" 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
w0=$(date +%s)
/root/r1-quiet.take.sh r1-merge "$WHAT" > /dev/null || exit 1
t0=$(date +%s)
LOG=$O/logs/$NAME-$(date -u +%Y%m%dT%H%M%SZ).txt
rc=99
trap 'rm -f /root/r1-quiet.lock; t1=$(date +%s); echo $t1 > /root/rmerge/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0)) rc=$rc waited=$((t0 - w0))" >> /root/rmerge/locktime.log' EXIT
trap "exit 130" INT TERM
echo "== $NAME: $WHAT; lock after $((t0 - w0)) s; $(date -u +%FT%TZ); limit $LIMIT s" > "$LOG"
setsid timeout -s TERM -k 30 "$LIMIT" "$@" >> "$LOG" 2>&1 < /dev/null
rc=$?
echo "== end rc=$rc $(date -u +%FT%TZ), $(( $(date +%s) - t0 )) s" >> "$LOG"
echo "$LOG rc=$rc"

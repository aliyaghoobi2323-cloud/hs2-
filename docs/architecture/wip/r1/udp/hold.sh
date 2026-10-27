#!/bin/bash
# hold.sh WHAT MINUTES: take the shared quiet lock (/root/r1-quiet.lock)
# atomically, retrying every RETRY s (default 60) while another agent holds it, and keep it
# until /root/rudp/hold.stop appears or MINUTES (at most 20) pass; then remove
# it (only if it is still ours). For interactive work under the lock (go test,
# builds, short rig checks): touch /root/rudp/hold.stop to end it early.
set -u
WHAT=${1:?WHAT}; MIN=${2:-20}; [ "$MIN" -le 20 ] || MIN=20
LOCK=/root/r1-quiet.lock; STOP=/root/rudp/hold.stop; LOG=/root/rudp/hold.log
rm -f "$STOP"
while ! ( set -C; echo "r1-udp $WHAT, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; do
  echo "$(date -u +%FT%TZ) lock held: $(cat "$LOCK" 2>/dev/null); retry in ${RETRY:-60} s" >> "$LOG"
  sleep "${RETRY:-60}"
done
drop() { grep -q "^r1-udp $WHAT, pid $$," "$LOCK" 2>/dev/null && rm -f "$LOCK"; echo "$(date -u +%FT%TZ) lock dropped: $WHAT" >> "$LOG"; }
trap 'drop; exit 130' INT TERM
echo "$(date -u +%FT%TZ) lock taken: $WHAT" >> "$LOG"
end=$(( $(date +%s) + MIN * 60 ))
while [ "$(date +%s)" -lt "$end" ] && [ ! -e "$STOP" ]; do sleep 2; done
drop
rm -f "$STOP"

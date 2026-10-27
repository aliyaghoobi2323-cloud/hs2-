#!/bin/bash
# block.sh WHAT LIST OUT: one rig block of r1-udp under the shared quiet lock
# (/root/r1-quiet.lock). It takes the lock atomically (set -C), retrying every
# 60 s while another agent holds it, runs every line of LIST (a command line,
# run with bash from this directory, OUT exported), stops at MAXMIN minutes
# (default 20), and always removes its own lock at the end.
set -u
D=$(cd "$(dirname "$0")" && pwd)
WHAT=${1:?WHAT}; LIST=${2:?LIST}; export OUT=${3:?OUT}
LOCK=/root/r1-quiet.lock
MAXMIN=${MAXMIN:-20}
mkdir -p "$OUT"
log() { echo "$(date -u +%FT%TZ) $*" >> "$OUT/block.log"; }
while ! ( set -C; echo "r1-udp $WHAT, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; do
  log "lock held: $(cat "$LOCK" 2>/dev/null); retry in 60 s"
  sleep 60
done
mine() { grep -q "^r1-udp $WHAT, pid $$," "$LOCK" 2>/dev/null; }
drop() { mine && rm -f "$LOCK"; }
trap 'drop; log "lock dropped (signal)"; exit 130' INT TERM
trap drop EXIT
T0=$(date +%s)
log "lock taken: $WHAT"
while IFS= read -r line; do
  line=${line%$'\r'}
  case "$line" in ''|'#'*) continue ;; esac
  if [ $(( $(date +%s) - T0 )) -ge $(( MAXMIN * 60 )) ]; then log "block time up, stopping before: $line"; break; fi
  log "run: $line"
  ( cd "$D" && bash -c "$line" ) >> "$OUT/block.log" 2>&1
  log "rc=$? for: $line"
done < "$LIST"
log "block done in $(( $(date +%s) - T0 )) s: $WHAT"

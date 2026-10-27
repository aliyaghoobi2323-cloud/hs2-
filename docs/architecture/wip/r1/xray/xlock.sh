#!/bin/bash
# xlock.sh WHAT CMD...: run CMD holding the shared rig's quiet lock
# (/root/r1-quiet.lock): take it atomically (set -C), retry every 60 s while
# another agent holds it, and remove it afterwards only if it is still ours.
# Each block must stay within 20 min (the caller's plan sizes it).
L=${LOCK:-/root/r1-quiet.lock}
what=${1:?usage: xlock.sh WHAT CMD...}; shift
while :; do
  if ( set -C; echo "r1-xray $what, pid $$, $(date -u +%FT%TZ)" > "$L" ) 2>/dev/null; then break; fi
  echo "$(date +%T) lock held: $(cat "$L" 2>/dev/null)"
  sleep 60
done
mine=$(cat "$L")
echo "$(date +%T) lock taken: $mine"
t0=$(date +%s)
"$@"
rc=$?
[ "$(cat "$L" 2>/dev/null)" = "$mine" ] && rm -f "$L"
echo "$(date +%T) lock released after $(( $(date +%s) - t0 )) s (rc $rc)"
exit $rc

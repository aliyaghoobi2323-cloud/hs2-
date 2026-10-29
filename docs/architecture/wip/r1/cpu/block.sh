#!/bin/bash
# block.sh WHAT CELLS OUT MINUTES REP... — one rig-lock block of the CPU-split
# A/B (R1 speed): waits for the shared lock in its fair queue
# (r1-quiet.take.sh r1-cpu WHAT), starts the test-side load (load.sh through
# r1-cpu.sh as r1cpu-load.scope, on the test cores), runs ab.sh with a
# deadline of MINUTES from the moment the lock was taken, stops the load and
# releases the lock (only if it is still ours).
set -u
WHAT=${1:?usage: block.sh WHAT CELLS OUT MINUTES REP...}; CELLS=$2; OUT=$3; MIN=$4; shift 4
mkdir -p "$OUT"
B=$OUT/block.log
echo "$(date +%T) waiting for the lock: $WHAT" >> "$B"
[ "$(/root/r1-quiet.take.sh r1-cpu "$WHAT")" = taken ] || { echo "$(date +%T) lock not taken" >> "$B"; exit 3; }
t0=$(date +%s)
echo "$(date +%T) lock taken" >> "$B"
release() {
  systemctl thaw r1cpu-load.scope 2>/dev/null
  systemctl stop r1cpu-load.scope 2>/dev/null
  if head -c 7 /root/r1-quiet.lock 2>/dev/null | grep -q '^r1-cpu '; then rm -f /root/r1-quiet.lock; fi
  echo "$(date +%T) load stopped, lock released ($(( $(date +%s) - t0 )) s)" >> "$B"
}
trap 'release; exit 130' INT TERM
LD=/root/rcpu/raw-load-$(date +%Y%m%dT%H%M%S)
R1CPU_UNIT=r1cpu-load SRC=/root/hs2-wt/r1-cpu/hs2-src /root/r1-cpu.sh /root/rcpu/load.sh "$LD" > "$OUT/load-scope.log" 2>&1 &
sleep 5
echo "$(date +%T) load: $(systemctl show r1cpu-load.scope -p ActiveState -p AllowedCPUs -p CPUWeight --value | paste -sd' ') logs $LD" >> "$B"
DEADLINE=$((t0 + MIN * 60 - 30)) /root/rcpu/ab.sh "$CELLS" "$OUT" "$@"
release

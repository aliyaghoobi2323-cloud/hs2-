#!/bin/bash
# chain.sh: the round-2 judge's remaining blocks, one after another, a gap
# of GAP s (240) after each lock is released: waits for block D1 to end,
# then E1, E2, D2 (block2.sh) and the DPI captures (capture.sh). One
# orchestrator: it refuses to start while another chain.sh runs.
set -u
GAP=${GAP:-240}
T=/root/r2j/tools
P=/root/r2j/plans
LOG=/root/r2j/chain.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
exec 9> /root/r2j/chain.flock
flock -n 9 || { echo "another chain.sh runs"; exit 1; }
log "chain pid $$ start"
until grep -q "done after" /root/r2j/D1/orch.log 2>/dev/null; do sleep 15; done
log "D1 done; gap"
sleep "$GAP"
TRACE_SS=1 "$T/block2.sh" E1 "$P/E1.txt" 3; log "E1 exit $?"
sleep "$GAP"
TRACE_SS=1 "$T/block2.sh" E2 "$P/E2.txt" 3; log "E2 exit $?"
sleep "$GAP"
"$T/block2.sh" D2 "$P/D2.txt" 2; log "D2 exit $?"
sleep "$GAP"
mkdir -p /root/r2j/raw-dpi
"$T/capture.sh" /root/r2j/raw-dpi "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old b1 b2 b2g b2bbrq; log "capture 1 exit $?"
"$T/capture.sh" /root/r2j/raw-dpi "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 96 old b1 b2; log "capture 2 exit $?"
"$T/capture.sh" /root/r2j/raw-dpi "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" 0 b2g b2gr; log "capture 3 exit $?"
log "chain done"

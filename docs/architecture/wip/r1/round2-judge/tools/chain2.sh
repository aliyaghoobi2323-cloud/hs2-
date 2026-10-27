#!/bin/bash
# chain2.sh: the round-2 judge's validation of the fixed candidate (b2jx and
# its variant b2jxr), one block after another with a gap of GAP s (240)
# after each lock is released: F1 (latency, duplex, loss; 3 repeats), F2
# (P10 at N=1, tunping at N=8; 3), F3 (N=2, N=64, the policer; 2), F4
# (N=300; 2), then the DPI captures. One orchestrator (flock).
set -u
GAP=${GAP:-240}
T=/root/r2j/tools
P=/root/r2j/plans
LOG=/root/r2j/chain.log
log() { echo "$(date +%T) $*" >> "$LOG"; }
exec 9> /root/r2j/chain.flock
flock -n 9 || { echo "another chain runs"; exit 1; }
log "chain2 pid $$ start"
TRACE_SS=1 "$T/block2.sh" F1 "$P/F1.txt" 3; log "F1 exit $?"
sleep "$GAP"
TRACE_SS=1 "$T/block2.sh" F2 "$P/F2.txt" 3; log "F2 exit $?"
sleep "$GAP"
"$T/block2.sh" F3 "$P/F3.txt" 2; log "F3 exit $?"
sleep "$GAP"
"$T/block2.sh" F4 "$P/F4.txt" 2; log "F4 exit $?"
sleep "$GAP"
mkdir -p /root/r2j/raw-dpi
"$T/capture.sh" /root/r2j/raw-dpi "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old b1 b2 b2jx b2bbrq; log "capture 1 exit $?"
"$T/capture.sh" /root/r2j/raw-dpi "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 96 old b1 b2 b2jx; log "capture 2 exit $?"
"$T/capture.sh" /root/r2j/raw-dpi "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" 0 b2jx b2jxr; log "capture 3 exit $?"
log "chain2 done"

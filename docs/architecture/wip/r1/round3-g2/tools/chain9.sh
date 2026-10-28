#!/bin/bash
# chain9.sh: the DPI capture of s5 (old, g2e; capture2.sh), block X3 (the
# per-connection policer on g2e), then the branch's tests (tests.sh).
set -u
L=/root/r3b/chain.log
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain9 $$ start"
CAP_S=24 /root/r3b/tools/capture2.sh /root/r3b/raw-dpi "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old g2e >> /root/r3b/dpi.nohup 2>&1
log "capture exit $?"
RS=/root/r3b/rig2 RLBIN=/root/r3b/rigload2 TRACE_SS=1 LIMIT=600 WAITMAX=14400 /root/r3b/tools/block5.sh X3 /root/r3b/plans/X3.txt 2 >> /root/r3b/X3.nohup 2>&1
log "block X3 exit $?"
/root/r3b/tools/tests.sh >> /root/r3b/tests.nohup 2>&1
log "tests exit $?"
log "chain9 $$ done"

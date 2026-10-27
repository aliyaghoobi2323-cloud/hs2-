#!/bin/bash
# chain7.sh (chain5.sh on block5.sh): γ2's last blocks (round 3), fewer and fuller (the rig is shared
# by more agents than planned): X1 = S3 and S5 (N = 2, 64, 300), X2 = S2b
# and S4 (two-way cells on g2e, 1% loss, the call), both on rig2 (the rig
# with the call flow), then the DPI capture of s5 (old, g2e).
set -u
L=/root/r3b/chain.log
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain7 $$ start"
for n in X1 X2; do
  RS=/root/r3b/rig2 RLBIN=/root/r3b/rigload2 TRACE_SS=1 LIMIT=1440 WAITMAX=14400 /root/r3b/tools/block5.sh $n /root/r3b/plans/$n.txt 2 >> /root/r3b/$n.nohup 2>&1
  log "block $n exit $?"
done
CAP_S=24 /root/r3b/tools/capture.sh /root/r3b/raw-dpi "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old g2e >> /root/r3b/dpi.nohup 2>&1
log "capture exit $?"
log "chain7 $$ done"

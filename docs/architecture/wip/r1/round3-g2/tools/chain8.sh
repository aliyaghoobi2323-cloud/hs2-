#!/bin/bash
# chain8.sh: after chain7 (its DPI capture), block X3 (the per-connection
# policer cell on g2e: S2 ran it on g2d, whose SIOCOUTQ measure held the gate
# shut for a round trip at each policer drop), then the branch's tests
# (tests.sh, each step once no quiet lock is held).
set -u
L=/root/r3b/chain.log
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain8 $$ start"
while pgrep -f "r3b/tools/chain7.sh" > /dev/null; do sleep 10; done
RS=/root/r3b/rig2 RLBIN=/root/r3b/rigload2 TRACE_SS=1 LIMIT=600 WAITMAX=14400 /root/r3b/tools/block5.sh X3 /root/r3b/plans/X3.txt 2 >> /root/r3b/X3.nohup 2>&1
log "block X3 exit $?"
/root/r3b/tools/tests.sh >> /root/r3b/tests.nohup 2>&1
log "tests exit $?"
log "chain8 $$ done"

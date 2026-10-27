#!/bin/bash
# chain3.sh: γ2's later blocks (round 3). Once no quiet lock is held: the
# rig copy rig2 (the worktree's rigload with the call flow, and its ana.py
# with CALL), its rigload built, and the budget's unit tests at the branch
# head; then S2 and S3 on the rig, S4 on rig2 (block2.sh: each waits for the
# lock and keeps 3 minutes after this candidate's block before).
set -u
L=/root/r3b/chain.log
W=/root/hs2-wt/r1-r3b
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain3 $$ start"
while [ -e /root/r1-quiet.lock ]; do sleep 20; done
rm -rf /root/r3b/rig2 && cp -r /root/r3b/rig /root/r3b/rig2 &&
  cp $W/docs/architecture/wip/rig-scale/rigload/main.go /root/r3b/rig2/rigload/main.go &&
  cp $W/docs/architecture/wip/rig-scale/ana.py /root/r3b/rig2/ana.py &&
  (cd /root/r3b/rig2/rigload && CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r3b/rigload2 .) && log "rig2 ready, rigload2 built"
(cd $W/hs2-src && nice /usr/local/go/bin/go test -count=1 -run "Budget|OWD|QuietCount|QuietDefaults|SendCap|ShapedConn|MoveRule|QuietThin" ./engine >> $L 2>&1; log "unit tests at $(git -C $W rev-parse --short HEAD) exit $?")
TRACE_SS=1 LIMIT=1440 /root/r3b/tools/block2.sh S2 /root/r3b/plans/S2.txt 2 >> /root/r3b/S2.nohup 2>&1; log "block S2 exit $?"
TRACE_SS=1 LIMIT=1440 /root/r3b/tools/block2.sh S3 /root/r3b/plans/S3.txt 2 >> /root/r3b/S3.nohup 2>&1; log "block S3 exit $?"
RS=/root/r3b/rig2 RLBIN=/root/r3b/rigload2 TRACE_SS=1 LIMIT=1440 /root/r3b/tools/block2.sh S4 /root/r3b/plans/S4.txt 2 >> /root/r3b/S4.nohup 2>&1; log "block S4 exit $?"
log "chain3 $$ done"

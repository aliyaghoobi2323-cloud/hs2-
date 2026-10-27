#!/bin/bash
# chain4.sh: γ2's last blocks (round 3) on g2e (the in-flight measure fix).
# Once no quiet lock is held: the budget's unit tests at the branch head,
# g2e built, rig2 (the call flow) and its rigload; then S1, S3, S2b on the
# rig, S4 on rig2, S5 (N=300) on the rig, and the DPI capture of s5 (old,
# g2e). block2.sh waits for the lock and keeps 3 minutes after this
# candidate's block before.
set -u
L=/root/r3b/chain.log
W=/root/hs2-wt/r1-r3b
log() { echo "$(date -u +%T) $*" >> $L; }
log "chain4 $$ start"
while [ -e /root/r1-quiet.lock ]; do sleep 20; done
cd $W/hs2-src
nice /usr/local/go/bin/go vet ./engine ./cmd/hs2 >> $L 2>&1
nice /usr/local/go/bin/go test -count=1 -run "Budget|OWD|QuietCount|QuietDefaults|SendCap|ShapedConn|MoveRule|QuietThin" ./engine >> $L 2>&1
log "unit tests at $(git -C $W rev-parse --short HEAD): exit $?"
CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r3b/bins/hs2-g2e ./cmd/hs2 && sha256sum /root/r3b/bins/hs2-g2e >> /root/r3b/bins-sha256.txt && log "built g2e ($(git -C $W rev-parse --short HEAD), $(git -C $W status --short hs2-src | wc -l) changed)"
rm -rf /root/r3b/rig2 && cp -r /root/r3b/rig /root/r3b/rig2 &&
  cp $W/docs/architecture/wip/rig-scale/rigload/main.go /root/r3b/rig2/rigload/main.go &&
  cp $W/docs/architecture/wip/rig-scale/ana.py /root/r3b/rig2/ana.py &&
  (cd /root/r3b/rig2/rigload && CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r3b/rigload2 .) && log "rig2 ready, rigload2 built"
blk() { # NAME REPS [env...]
  local n=$1 r=$2; shift 2
  env "$@" TRACE_SS=1 LIMIT=1440 /root/r3b/tools/block2.sh "$n" "/root/r3b/plans/$n.txt" "$r" >> /root/r3b/$n.nohup 2>&1
  log "block $n exit $?"
}
blk S1 2
blk S3 2
blk S2b 2
blk S4 2 RS=/root/r3b/rig2 RLBIN=/root/r3b/rigload2
blk S5 2
CAP_S=24 /root/r3b/tools/capture.sh /root/r3b/raw-dpi "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old g2e >> /root/r3b/dpi.nohup 2>&1
log "capture exit $?"
log "chain4 $$ done"

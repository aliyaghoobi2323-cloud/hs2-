#!/bin/bash
# chain.sh NAME...: R1 round 5's measuring blocks in order, each through
# block.sh (3 reps; the fair rig lock, at most 24 min, GAP between them).
T=/root/hs2-wt/r1-r5/docs/architecture/wip/r1/round5
for b in "$@"; do
	"$T/tools/block.sh" "$b" "$T/plans/$b.txt" 3
	"$T/tools/assemble.sh" "$b" >> /root/r5/chain.log 2>&1
	echo "$(date -u +%FT%TZ) $b done" >> /root/r5/chain.log
done

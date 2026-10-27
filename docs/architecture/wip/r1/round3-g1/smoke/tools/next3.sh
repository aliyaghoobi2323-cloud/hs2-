#!/bin/bash
# next3.sh: blocks F2, F3 and F4 of g1e in turn (block3.sh: takes the lock
# itself, polling every 2 s; 200 s between my blocks).
set -u
LOG=/root/r3a/next.log
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
cd /root/r3a
tools/block3.sh F2 plans/F2.txt 2 plans/F2z.txt 1
log "F2 done"
sleep 200
tools/block3.sh F3 plans/F3.txt 2 plans/F3z.txt 1
log "F3 done"
sleep 200
tools/block3.sh F4 plans/F4.txt 2 plans/F4z.txt 1
log "F4 done"

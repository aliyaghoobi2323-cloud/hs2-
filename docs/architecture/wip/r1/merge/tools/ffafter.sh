#!/bin/bash
# ffafter.sh LOGGLOB: once the block log matching LOGGLOB has its end line, fast-forward r1-merge to r1-merge-next
for i in $(seq 1 720); do f=$(ls $1 2>/dev/null | tail -1); [ -n "$f" ] && grep -q "^== end" "$f" && break; sleep 2; done
cd /root/hs2-wt/r1-merge && git merge --ff-only r1-merge-next > /root/rmerge/ffafter.log 2>&1; echo "rc $? $(date -u +%FT%TZ) head $(git rev-parse --short HEAD)" >> /root/rmerge/ffafter.log

#!/bin/bash
# s23.sh: S2 (REPS 2) and S3 (REPS 1, diagnosis) in one locked block, then ana.py and the class tables
cd /root/rmix || exit 1
LIMIT=1200 tools/mblock.sh S2 /root/rmix/plans/S2.txt /root/rmix/plans/S3.txt 1 || echo "$(date -u +%T) block S2+S3 gave up" >> chain.log
if [ -s S2/res.jsonl ]; then
  python3 rig/ana.py S2/res.jsonl > S2/ana.txt 2>&1
  python3 tools/classtable.py rig S2/res.jsonl > S2/table.md 2>&1
fi
echo "$(date -u +%T) block S2+S3 done" >> chain.log

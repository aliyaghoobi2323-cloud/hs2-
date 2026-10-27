#!/bin/bash
# mchain.sh: the classes smoke's blocks one after another (each takes and
# releases the quiet lock itself, mblock.sh), with a 3 minute pause between
# blocks so the other agents get the rig too; after each block, ana.py and the
# class tables into /root/rmix/NAME.
set -u
cd /root/rmix || exit 1
for b in "$@"; do
  LIMIT=1200 tools/mblock.sh "$b" "/root/rmix/plans/$b.txt" || echo "$(date -u +%T) block $b gave up" >> chain.log
  if [ -s "$b/res.jsonl" ]; then
    python3 rig/ana.py "$b/res.jsonl" > "$b/ana.txt" 2>&1
    python3 tools/classtable.py rig "$b/res.jsonl" > "$b/table.md" 2>&1
  fi
  echo "$(date -u +%T) block $b done" >> chain.log
  [ "$b" = "${!#}" ] || sleep 180
done

#!/bin/bash
# chain.sh "NAME REPS"...: the judge's blocks one after another (block.sh NAME
# /root/r4j/plans/NAME.txt REPS: each waits its GAP and takes the lock
# through the fair queue)
for a in "$@"; do
  set -- $a
  echo "$(date +%T) $1 start" >> /root/r4j/chain.log
  /root/r4j/tools/block.sh "$1" "/root/r4j/plans/$1.txt" "${2:-2}"
  echo "$(date +%T) $1 done ($?)" >> /root/r4j/chain.log
done

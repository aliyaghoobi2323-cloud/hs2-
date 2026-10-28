#!/bin/bash
# chain.sh NAME...: the round-4 regression blocks one after another (block.sh
# NAME /root/r4/plans/NAME.txt 3: each waits its GAP and takes the lock
# through the fair queue); a DPI name runs capture.sh instead.
for n in "$@"; do
  echo "$(date +%T) $n" >> /root/r4/chain.log
  case "$n" in
    DPI*) /root/r4/tools/capture.sh "$n" ;;
    *) /root/r4/tools/block.sh "$n" /root/r4/plans/$n.txt 3 ;;
  esac
  echo "$(date +%T) $n done ($?)" >> /root/r4/chain.log
done

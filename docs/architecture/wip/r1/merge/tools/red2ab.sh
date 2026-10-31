#!/bin/bash
# red2ab.sh: TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo before and after
# the fix of its wait, each next to six spinning loops on the test cores.
# Binaries built once from /root/rmerge/wt-red2; each arm 300 runs.
set -u
O=/root/rmerge/suite/red2ab.txt
cd /root/rmerge/wt-red2/hs2-src/engine || exit 1
for round in 1 2; do
 for arm in old new; do
  /root/r1-cpu.sh timeout 700 sh -c "for i in 1 2 3 4 5 6; do (while :; do :; done) & done; wait" > /dev/null 2>&1 &
  HP=$!
  t0=$(date +%s)
  /root/r1-cpu.sh /root/rmerge/raw-bins/red2-$arm.test -test.count=300 -test.timeout=650s -test.v -test.run '^TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo$' > /root/rmerge/raw-suite/red2ab-$arm-$round.txt 2>&1
  r=$?
  kill "$HP" 2> /dev/null
  wait "$HP" 2> /dev/null
  f=/root/rmerge/raw-suite/red2ab-$arm-$round.txt
  echo "$arm round $round: rc $r, $(( $(date +%s) - t0 )) s, pass $(grep -c '^--- PASS' $f), fail $(grep -c '^--- FAIL' $f)" >> "$O"
  grep -a 'the link died holding\|rig tested nothing\|outlived\|relays let go\|a pause of' "$f" | sed 's/^ *[a-z0-9_.]*:[0-9]*: //' | sort | uniq -c | cut -c1-140 >> "$O"
  grep -ao 'borrowed [0-9]* KiB, [0-9]* held\|borrowed [0-9]* KiB; held' "$f" | sort | uniq -c >> "$O"
 done
done
echo "end $(date -u +%FT%TZ)" >> "$O"

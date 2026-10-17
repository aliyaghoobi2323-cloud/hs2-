#!/bin/bash
# runplan.sh PLAN OUT.jsonl: runs a plan's lines one at a time (resumable: a line whose id is in OUT is skipped).
# plan line: <id> <bin> ENV=V ...
J=$(cd "$(dirname "$0")" && pwd)
PLAN=$1; OUT=$2
touch $OUT
grep -v '^#' $PLAN | grep -v '^\s*$' | while read -r id bin rest; do
  grep -q "\"id\":\"$id\"" $OUT && continue
  echo "$(date +%T) start $id load $(cut -d' ' -f1 /proc/loadavg)" >> $OUT.log
  j=$($J/rig/run1.sh $bin $rest 2>&1 < /dev/null | grep -o '{"scen.*' | tail -1)
  if [ -n "$j" ]; then
    echo "{\"id\":\"$id\",\"r\":$j}" >> $OUT
  else
    echo "$(date +%T) FAIL $id" >> $OUT.log
  fi
done
echo "$(date +%T) done $PLAN" >> $OUT.log

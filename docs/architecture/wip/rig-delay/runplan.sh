#!/bin/bash
# runplan.sh PLAN OUT.jsonl: runs a plan's lines one at a time (resumable: a line whose id is in OUT is skipped).
# plan line: <id> <bin> ENV=V ...
# Before each run it waits (up to 60 s) while the machine's 1-minute load is over 8 (another agent measures here too).
J=$(cd "$(dirname "$0")" && pwd)
PLAN=$1; OUT=$2
touch $OUT
grep -v '^#' $PLAN | grep -v '^\s*$' | while read -r id bin rest; do
  grep -q "\"id\":\"$id\"" $OUT && continue
  w=0
  while [ $w -lt 60 ] && awk '{exit !($1 > 8)}' /proc/loadavg; do sleep 10; w=$((w+10)); done
  echo "$(date +%T) start $id load $(cut -d' ' -f1 /proc/loadavg) waited ${w}s" >> $OUT.log
  j=$($J/rig/run1.sh $bin $rest 2>&1 < /dev/null | grep -o '{"scen.*' | tail -1)
  if [ -n "$j" ]; then
    echo "{\"id\":\"$id\",\"r\":$j}" >> $OUT
  else
    echo "$(date +%T) FAIL $id" >> $OUT.log
  fi
done
echo "$(date +%T) done $PLAN" >> $OUT.log

#!/bin/bash
# mon.sh LAB_ID OUT : every 5 s the Iran hs2 process' cpu ticks (utime+stime) and RSS KB
ID=$1; OUT=$2
while :; do
  for p in $(ip netns pids ir$ID 2>/dev/null); do
    case "$(readlink /proc/$p/exe 2>/dev/null)" in */hs2_*|*/hs2) ;; *) continue;; esac
    s=$(cut -d' ' -f14,15 /proc/$p/stat 2>/dev/null); r=$(awk '/VmRSS/{print $2}' /proc/$p/status 2>/dev/null)
    [ -n "$s" ] && echo "$(date +%s.%N) $p $s $r" >> $OUT
  done; sleep 5; done

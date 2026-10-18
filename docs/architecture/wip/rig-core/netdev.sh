#!/bin/bash
# netdev.sh: every 2 s, the TUN device's counters (rx/tx drops included) of each running rig test, with its run id
S=/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z11
while true; do
  for p in $(pgrep -f 'bin/(core|base)[0-9]?\.test'); do
    id=$(tr '\0' '\n' < /proc/$p/environ 2>/dev/null | grep -E '^Z5_(SCEN|VER|REP)=' | cut -d= -f2 | paste -sd_ -)
    l=$(grep z5d /proc/$p/net/dev 2>/dev/null)
    [ -n "$l" ] && echo "$(date +%T) $id $l"
  done
  sleep 2
done

#!/bin/bash
# seq146.sh "T1 T2" "T3 T4" ... — run under labslot.sh --all: each argument is a pair run at once,
# first on LAB_ID 30 cores 0-9, second on LAB_ID 31 cores 10-19.
cd /opt/hs2lab/rs146
for pair in "$@"; do set -- $pair
  LAB_ID=30 taskset -c 0-9 bash rs146.sh $1 & a=$!
  if [ -n "${2:-}" ]; then sleep 2; LAB_ID=31 taskset -c 10-19 bash rs146.sh $2 & b=$!; wait $b; fi
  wait $a
done
f=/run/lab3g-backlog.old; [ -f $f ] && { sysctl -qw net.core.netdev_max_backlog="$(cat $f)"; rm -f $f; }
echo "seqdone $(date -u +%FT%TZ)" > /opt/hs2lab/rs146/SEQDONE.$$

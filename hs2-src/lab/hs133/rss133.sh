#!/bin/bash
# rss133.sh OUTDIR: RSS (MB) of the hs133 binaries in ir$LAB_ID / kh$LAB_ID every 5 s
set -u
O=$1; F=${2:-rss.txt}; T0=${3:-0}
for i in $(seq 1 80); do
  sleep 5; [ -d "$O" ] || break
  for ns in ir kh; do
    r=0
    for p in $(ip netns pids $ns${LAB_ID:-} 2>/dev/null); do
      case "$(readlink /proc/$p/exe 2>/dev/null)" in
        /tmp/d133-*/hs2*) r=$(awk '/VmRSS/{print int($2/1024)}' /proc/$p/status 2>/dev/null);;
      esac
    done
    echo "$((T0+i*5)) $ns ${r:-0}"
  done >> "$O/$F"
done

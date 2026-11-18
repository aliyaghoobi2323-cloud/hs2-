#!/bin/bash
cd /opt/hs2lab/ul268s
for n in ${PN:-FN2 CA3 CN3 FN3 FA3}; do
  while [ ! -e runs/$n/t0 ]; do sleep 2; done
  sleep 40
  ip netns exec ir30 curl -s -m 90 -o runs/$n/allocs.prof "http://127.0.0.1:6060/debug/pprof/allocs?seconds=60"
  echo "$(date -u +%T) $n allocs $(stat -c %s runs/$n/allocs.prof 2>/dev/null)" >> prof.log
done

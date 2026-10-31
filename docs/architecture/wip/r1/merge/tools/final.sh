#!/bin/bash
# final.sh: the step-5 suite on the final head under the R1 CPU rule
# (docs/architecture/wip/r1/cpu/REPORT.fa.md): every go test, -race, fuzz
# and build through /root/r1-cpu.sh (cores 6-11, low priority), no rig lock;
# a full-module run holds flock /root/r1-tests.lock (fixed loopback
# addresses collide between two suites). One log per step in /root/rmerge/logs.
set -u
T=/root/rmerge/tools
L=/root/rmerge/final.log
step() { # step NAME CMD...
  local n=$1; shift
  local log=/root/rmerge/logs/$n-$(date -u +%Y%m%dT%H%M%SZ).txt t0=$(date +%s)
  echo "== $n: $* ($(date -u +%FT%TZ))" > "$log"
  flock /root/r1-tests.lock /root/r1-cpu.sh env STEP5_IGNORE_LOCK=1 NO_UDP_AB=1 "$@" >> "$log" 2>&1 < /dev/null
  local rc=$?
  echo "== end rc=$rc $(date -u +%FT%TZ), $(( $(date +%s) - t0 )) s" >> "$log"
  echo "$n rc=$rc $(( $(date +%s) - t0 )) s $log" >> "$L"
}
echo "start $(date -u +%FT%TZ) head $(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)" >> "$L"
step f-release "$T/blocks.sh" release
step f-check "$T/blocks.sh" check
step f-race386 "$T/blocks.sh" race386
step f-fuzzkernel "$T/blocks.sh" fuzzkernel
step f-arm7 "$T/blocks.sh" arm7
step f-install "$T/blocks.sh" install
step f-flakes "$T/blocks.sh" flakes
echo "end $(date -u +%FT%TZ)" >> "$L"

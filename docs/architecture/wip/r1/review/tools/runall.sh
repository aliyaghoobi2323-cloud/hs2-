#!/bin/bash
# runall.sh OUT PROD_BIN R1_BIN [SCENARIO...] — the R1 review's (part C)
# runbook scenarios one after the other, each with fieldug.sh in its own
# fresh namespaces. Run inside hs2-src/install/tests/sandbox.sh:
#   bash hs2-src/install/tests/sandbox.sh bash docs/architecture/wip/r1/review/tools/runall.sh OUT PROD R1
# Default scenarios (NAME=DIRECTION:SCENARIO):
#   up-e07=up:e-first:0.7     the runbook: Iran, Kharej 0.7 s later
#   up-x03=up:x-first:0.3     the order inverted by 0.3 s
#   up-both=up:both           both at once (the red team's 0.3 s)
#   up-xflap=up:xflap:0.7     Kharej's new binary dies once: Kharej reverts, then Iran by the runbook's rule
#   up-erefuse=up:erefuse:0.7 Iran's safe-apply refuses, Kharej goes alone
#   back-e07=back:e-first:0.7 the runbook's rollback: R1 to production, Iran first
# and the review's second batch, given on the command line:
#   up-e03=up:e-first:0.3 up-x06=up:x-first:0.6 up-x10=up:x-first:1.0
#   up-x20=up:x-first:2.0 up-xflapn=up:xflapn:0.3
# Then tools/gap.py OUT: each run's service gap (first restart to the first
# new connection served after both).
set -u
[ "${HS2_SANDBOX:-}" = proven ] || { echo "runall.sh: run me inside sandbox.sh" >&2; exit 2; }
OUT=$1; P=$2; R=$3; shift 3
T=$(cd "$(dirname "$0")" && pwd)
[ $# -gt 0 ] || set -- up-e07=up:e-first:0.7 up-x03=up:x-first:0.3 up-both=up:both \
	up-xflap=up:xflap:0.7 up-erefuse=up:erefuse:0.7 back-e07=back:e-first:0.7
mkdir -p "$OUT"
for s in "$@"; do
	name=${s%%=*}; rest=${s#*=}; dir=${rest%%:*}; scen=${rest#*:}
	if [ "$dir" = up ]; then from=$P; to=$R; else from=$R; to=$P; fi
	echo "=== $name ($dir $scen) $(date -u +%T)" | tee -a "$OUT/runall.log"
	bash "$T/fieldug.sh" "$OUT/$name" "$from" "$to" "$scen" >> "$OUT/runall.log" 2>&1
	echo "rc=$?" >> "$OUT/runall.log"
	sleep 3
done
echo "=== done $(date -u +%T)" | tee -a "$OUT/runall.log"

#!/bin/bash
# speed.sh OUTDIR ROUND MODE... — one round of the test-speed runs (R1 speed,
# item 3): suite.sh for each MODE in turn (p1 p4 p8 split:3 split:6 ...,
# labels MODE-rROUND), and fuzz:serial / fuzz:par (step 5's fuzz.sh, every
# target 60 s: one after another with -parallel 4, or FUZZ_JOBS=3 with
# -parallel 2), all on the test cores through r1-cpu.sh, no rig lock (the
# A/B passed: docs/architecture/wip/r1/cpu/REPORT.fa.md). 20 s between runs.
set -u
OUT=${1:?usage: speed.sh OUTDIR ROUND MODE...}; R=$2; shift 2
mkdir -p "$OUT"
export SLOW_LIST=${SLOW_LIST:-/root/rcpu/slow-ref.txt}
FZ=/root/hs2-wt/r1-cpu/docs/architecture/wip/r1/merge-prep/step5/fuzz.sh
for m in "$@"; do
  l=$(echo "$m-r$R" | tr ':' '-')
  echo "$(date +%T) start $l" >> "$OUT/speed.log"
  case "$m" in
    fuzz:serial|fuzz:par)
      t0=$(date +%s.%N)
      if [ "$m" = fuzz:par ]; then j=3; p=2; else j=1; p=4; fi
      grep '^cpu[0-9]' /proc/stat > "$OUT/$l.cpus-start"
      ( while :; do echo "$(date +%s) $(head -c 60 /root/r1-quiet.lock 2>/dev/null | tr '\n' ' ')"; sleep 5; done ) > "$OUT/$l.lock" 2>&1 &
      smp=$!
      STEP5_IGNORE_LOCK=1 STEP5_OUT=$OUT/fuzz FUZZ_JOBS=$j FUZZ_PARALLEL=$p /root/r1-cpu.sh "$FZ" > "$OUT/$l.out" 2>&1
      rc=$?
      kill $smp 2>/dev/null
      grep '^cpu[0-9]' /proc/stat > "$OUT/$l.cpus-end"
      echo "$l $m start $t0 end $(date +%s.%N) wall $(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b - a}') rc $rc head fuzz" >> "$OUT/$l.meta" ;;
    *)
      /root/rcpu/suite.sh "$OUT" "$l" "$m" > /dev/null 2>&1 ;;
  esac
  echo "$(date +%T) done $l $(tail -1 "$OUT/$l.meta" 2>/dev/null | grep -o 'wall [0-9.]* rc .*head' )" >> "$OUT/speed.log"
  sleep 20
done
echo "$(date +%T) round $R end" >> "$OUT/speed.log"

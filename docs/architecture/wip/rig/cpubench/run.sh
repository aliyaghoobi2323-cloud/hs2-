#!/bin/bash
# run.sh OUT REPS: interleaved cpubench runs (B N P, 1 and 8 streams), one JSON line each
cd "$(dirname "$0")"
OUT=$1; REPS=$2
for rep in $(seq 1 $REPS); do
  for st in 1 8; do
    mib=$((1024 / st))
    for v in B N P; do
      ./cpubench -v $v -streams $st -mib $mib -label "s$st|$v|$rep" >> $OUT
    done
  done
done

#!/bin/bash
# suite.sh OUTDIR LABEL MODE — one timed run of hs2's Go test suite (R1 speed,
# item 3), on the test cores through r1-cpu.sh. MODE:
#   p1 | p4 | p8   go test -json -count=1 -p N -timeout 30m ./... (hs2-src)
#   engine         go test -json -count=1 -timeout 30m ./engine alone
#   split:K        the two-class layout: class (a) (every test not in
#                  SLOW_LIST) with -p 6 on all test cores first, then class (b)
#                  (SLOW_LIST: "package test" lines) in K shards at once, each
#                  serial (-p 1) and pinned to its own share of the test cores
#                  (K=3: 6-7, 8-9, 10-11; K=6: one core each); shards are
#                  balanced by the tests' elapsed time in SLOW_LIST (third
#                  column, longest first onto the lightest shard)
# Writes OUTDIR/LABEL.json (go test -json; split: LABEL-a.json, LABEL-bK.json),
# OUTDIR/LABEL.meta (start, end, wall s, exit codes), OUTDIR/LABEL.lock (the
# rig lock's holder every 5 s: who else was on the host) and per-CPU
# /proc/stat at start and end (LABEL.cpus-start/end).
set -u
OUT=${1:?usage: suite.sh OUTDIR LABEL MODE}; L=${2:?}; MODE=${3:?}
SRC=${SRC:-/root/hs2-wt/r1-cpu/hs2-src}
CPU=${CPU:-/root/r1-cpu.sh}
command -v go >/dev/null || export PATH=$PATH:/usr/local/go/bin
mkdir -p "$OUT"
cd "$SRC" || exit 2
( while :; do echo "$(date +%s) $(head -c 60 /root/r1-quiet.lock 2>/dev/null | tr '\n' ' ')"; sleep 5; done ) > "$OUT/$L.lock" 2>&1 &
smp=$!
trap 'kill $smp 2>/dev/null' EXIT
grep '^cpu[0-9]' /proc/stat > "$OUT/$L.cpus-start"
t0=$(date +%s.%N)
rcs=""
case "$MODE" in
  p[0-9]*)
    "$CPU" go test -json -count=1 -p "${MODE#p}" -timeout 30m ./... > "$OUT/$L.json" 2> "$OUT/$L.err"
    rcs=$? ;;
  engine)
    "$CPU" go test -json -count=1 -timeout 30m ./engine > "$OUT/$L.json" 2> "$OUT/$L.err"
    rcs=$? ;;
  split:*)
    K=${MODE#split:}
    SLOW=${SLOW_LIST:?SLOW_LIST: package test elapsed lines}
    # class (a): everything but the slow tests, all test cores, -p 6
    skip="^($(awk '{print $2}' "$SLOW" | sort -u | paste -sd'|'))\$"
    "$CPU" go test -json -count=1 -p 6 -timeout 30m -skip "$skip" ./... > "$OUT/$L-a.json" 2> "$OUT/$L-a.err"
    rcs="a:$?"
    echo "$(date +%s.%N) class a done" >> "$OUT/$L.meta"
    # class (b): K shards, longest first onto the lightest shard
    python3 - "$SLOW" "$K" "$OUT/$L" <<'P'
import sys
rows = [l.split() for l in open(sys.argv[1]) if l.strip()]
k = int(sys.argv[2])
rows.sort(key=lambda r: -float(r[2]))
load = [0.0] * k
sh = [[] for _ in range(k)]
for p, t, e in rows:
    i = load.index(min(load))
    load[i] += float(e)
    sh[i].append((p, t))
for i in range(k):
    with open("%s-shard%d.txt" % (sys.argv[3], i), "w") as f:
        for p, t in sh[i]:
            f.write("%s %s\n" % (p, t))
    print("shard %d: %d tests, %.0f s" % (i, len(sh[i]), load[i]))
P
    first=6; per=$((6 / K)); pids=()
    for i in $(seq 0 $((K - 1))); do
      c0=$((first + i * per)); c1=$((c0 + per - 1))
      f=$OUT/$L-shard$i.txt
      # packages as import paths (go test -json) or relative to hs2-src (slow-tests.txt)
      pk=$(awk '{print $1}' "$f" | sort -u | sed 's|^github.com/[^/]*/[^/]*/||; s|^github.com/[^/]*/[^/]*$|.|; s|^[^.]|./&|' | paste -sd' ')
      re="^($(awk '{print $2}' "$f" | sort -u | paste -sd'|'))\$"
      TEST_CPUS=$c0-$c1 R1CPU_UNIT= "$CPU" go test -json -count=1 -p 1 -timeout 30m -run "$re" $pk > "$OUT/$L-b$i.json" 2> "$OUT/$L-b$i.err" &
      pids+=($!)
    done
    for i in "${!pids[@]}"; do wait "${pids[$i]}"; rcs="$rcs b$i:$?"; done ;;
  *) echo "bad MODE $MODE" >&2; exit 2 ;;
esac
t1=$(date +%s.%N)
grep '^cpu[0-9]' /proc/stat > "$OUT/$L.cpus-end"
echo "$L $MODE start $t0 end $t1 wall $(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.1f", b - a}') rc $rcs head $(git -C "$SRC" rev-parse --short HEAD)" >> "$OUT/$L.meta"
tail -1 "$OUT/$L.meta"

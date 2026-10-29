#!/bin/bash
# load.sh LOGDIR — the heavy test-side load of the CPU-split A/B (R1 speed):
# three loops at once, each restarting when it ends, so the test cores stay
# full: the engine package's tests under -race, a fuzz target (go test -fuzz,
# one worker a CPU) and a full rebuild of hs2 (go build -a, its own cache).
# Started through r1-cpu.sh (R1CPU_UNIT=r1cpu-load), so it runs only on the
# test cores; ab.sh freezes the scope for the quiet runs and thaws it for
# the others (systemctl freeze/thaw r1cpu-load.scope); stopped with
# systemctl stop r1cpu-load.scope.
set -u
L=${1:?usage: load.sh LOGDIR}
SRC=${SRC:-/root/hs2-wt/r1-cpu/hs2-src}
command -v go >/dev/null || export PATH=$PATH:/usr/local/go/bin
mkdir -p "$L"
( while :; do echo "race start $(date +%T)"; (cd "$SRC" && go test -race -count=1 -timeout 30m ./engine/ 2>&1 | tail -3); done ) > "$L/race.log" 2>&1 &
( while :; do echo "fuzz start $(date +%T)"; (cd "$SRC/third_party/smux" && go test -run '^$' -fuzz '^FuzzSessionRecv$' -fuzztime 120s . 2>&1 | tail -2); done ) > "$L/fuzz.log" 2>&1 &
( while :; do echo "build start $(date +%T)"; (cd "$SRC" && GOCACHE="$L/gocache" go build -a -o "$L/hs2-load" ./cmd/hs2 2>&1 | tail -2); done ) > "$L/build.log" 2>&1 &
wait

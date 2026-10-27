#!/bin/bash
# capture.sh OUT "CELL" SNAP LABEL... : γ2's DPI captures (the round-2
# judge's capture.sh, on γ2's rig copy, RIG_ID bd). For each binary label
# (old = OLD_BIN, the rest from BINS), one rig run of CELL with tcpdump on
# the bottleneck namespace's device mx (both directions; link connections
# on port 2096 only), from the moment the device is up, CAP_S seconds (24).
# SNAP 0: whole packets (TLS records are extracted with tlsrec.py
# afterwards, then the pcap is deleted); SNAP 96: headers only. Takes the
# quiet lock atomically (waits, polling each 60 s, while anyone holds it);
# nothing leaves the rig namespaces.
set -u
O=${1:?}; CELL=${2:?}; SNAP=${3:?}; shift 3
RS=/root/r3b/rig
CAP_S=${CAP_S:-24}
LOCK=/root/r1-quiet.lock
BINS=${BINS:-/root/r3b/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3b/bins/hs2-base}
mkdir -p "$O"
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
w=0
until [ $(( $(date +%s) - $(cat /root/r3b/last-block-end 2>/dev/null || echo 0) )) -ge 180 ] && ! busy &&
  ( set -C; echo "r1-r3b γ2 DPI capture, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; do
  [ $w -ge 3600 ] && { echo "no lock after ${w}s"; exit 1; }
  sleep 5; w=$((w + 5))
done
t0=$(date +%s)
trap 'rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3b/last-block-end; echo "capture-$(basename "$O") $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3b/locktime.log' EXIT
for b in "$@"; do
  t=$b-$(echo "$CELL" | md5sum | cut -c1-6)
  echo "$CELL BIN=$b" > "$O/plan-$t.txt"
  ip netns del r-midbd 2>/dev/null
  mkdir -p "$O/run-$t"; [ -x "$O/run-$t/rigload" ] || cp /root/r3b/rigload "$O/run-$t/rigload"
  REPS=1 RIG_ID=bd BINS=$BINS OLD_BIN=$OLD_BIN "$RS/run.sh" "$O/plan-$t.txt" "$O/run-$t" > "$O/run-$t.out" 2>&1 &
  rp=$!
  # mx up (not only made): tcpdump on a device that is not up yet fails
  for i in $(seq 1 400); do ip netns exec r-midbd ip link show mx 2>/dev/null | grep -q ",UP" && break; sleep 0.05; done
  ip netns exec r-midbd timeout "$CAP_S" tcpdump -i mx -s "$SNAP" -B 65536 -w "$O/cap-$t.pcap" "tcp port 2096" > "$O/tcpdump-$t.txt" 2>&1
  wait $rp
  echo "$(date +%T) $t done" >> "$O/capture.log"
done

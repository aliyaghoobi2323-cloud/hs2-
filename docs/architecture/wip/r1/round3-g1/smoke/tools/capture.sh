#!/bin/bash
# capture.sh OUT "CELL" SNAP LABEL... : DPI captures of R1 round 3, g1 (the
# round-2 judge's capture.sh, for rig RIG_ID ad and /root/r3a). For each
# binary label (old = OLD_BIN, the rest from BINS), one rig run of CELL with
# tcpdump on the bottleneck namespace's device mx (both directions; link
# connections on port 2096 only), from the moment the device is up, CAP_S
# seconds (24). SNAP 0: whole packets (records extracted with tlsrec.py,
# then the pcap is deleted). Takes /root/r1-quiet.lock atomically (waits
# while another holds it), holds it for all the labels, releases it at once.
# Nothing leaves the rig namespaces.
set -u
O=${1:?}; CELL=${2:?}; SNAP=${3:?}; shift 3
RS=/root/r3a/rig
CAP_S=${CAP_S:-24}
LOCK=/root/r1-quiet.lock
BINS=${BINS:-/root/r3a/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r3a/bins/hs2-old}
mkdir -p "$O"
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
w=0
while :; do
  if ! busy && ( set -C; echo "r1-r3a γ1 DPI capture, pid $$, $(date -u +%FT%TZ)" > "$LOCK" ) 2>/dev/null; then break; fi
  if [ $w -ge 5400 ]; then echo "no lock after ${w}s"; exit 1; fi
  sleep 60; w=$((w + 60))
done
trap 'rm -f "$LOCK"' EXIT
trap 'exit 130' INT TERM
echo "$(date -u +%FT%TZ) lock taken after ${w}s" >> "$O/capture.log"
for b in "$@"; do
  t=$b-$(echo "$CELL" | md5sum | cut -c1-6)
  echo "$CELL BIN=$b" > "$O/plan-$t.txt"
  ip netns del r-midad 2>/dev/null
  mkdir -p "$O/run-$t"; [ -x "$O/run-$t/rigload" ] || cp /root/r3a/rigload "$O/run-$t/rigload"
  REPS=1 RIG_ID=ad BINS=$BINS OLD_BIN=$OLD_BIN "$RS/run.sh" "$O/plan-$t.txt" "$O/run-$t" > "$O/run-$t.out" 2>&1 &
  rp=$!
  for i in $(seq 1 400); do ip netns exec r-midad ip link show mx 2>/dev/null | grep -q ",UP" && break; sleep 0.05; done
  ip netns exec r-midad timeout "$CAP_S" tcpdump -i mx -s "$SNAP" -B 65536 -w "$O/cap-$t.pcap" "tcp port 2096" > "$O/tcpdump-$t.txt" 2>&1
  wait $rp
  echo "$(date -u +%FT%TZ) $t done" >> "$O/capture.log"
done
echo "$(date -u +%FT%TZ) lock released" >> "$O/capture.log"

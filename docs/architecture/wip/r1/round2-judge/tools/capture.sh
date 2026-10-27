#!/bin/bash
# capture.sh OUT "CELL" SNAP LABEL... : the round-2 judge DPI captures. For
# each binary label (old = OLD_BIN, the rest from BINS), one rig run of CELL
# with tcpdump on the bottleneck namespace's device mx (both directions: the
# upload leaving, the download arriving; link connections on port 2096
# only), from the moment the
# namespace exists, CAP_S seconds (24). SNAP 0: whole packets (TLS records are
# extracted with tlsrec.py afterwards, then the pcap is deleted); SNAP 96:
# headers only (connection count, SYN times, bytes). Holds
# /root/r1-quiet.lock. Nothing leaves the rig namespaces.
set -u
O=${1:?}; CELL=${2:?}; SNAP=${3:?}; shift 3
RS=/root/r2j/rig
CAP_S=${CAP_S:-24}
LOCK=/root/r1-quiet.lock
BINS=${BINS:-/root/r2j/bins.txt}; OLD_BIN=${OLD_BIN:-/root/r2j/bins/hs2-base}
mkdir -p "$O"
[ -e "$LOCK" ] && { echo "lock held: $(cat $LOCK)"; exit 1; }
echo "r1-r2j judge DPI capture, pid $$, $(date -Is)" > "$LOCK"
trap "rm -f $LOCK" EXIT
for b in "$@"; do
  t=$b-$(echo "$CELL" | md5sum | cut -c1-6)
  echo "$CELL BIN=$b" > "$O/plan-$t.txt"
  ip netns del r-midjd 2>/dev/null
  mkdir -p "$O/run-$t"; [ -x "$O/run-$t/rigload" ] || cp /root/r2j/rigload "$O/run-$t/rigload"
  REPS=1 RIG_ID=jd BINS=$BINS OLD_BIN=$OLD_BIN "$RS/run.sh" "$O/plan-$t.txt" "$O/run-$t" > "$O/run-$t.out" 2>&1 &
  rp=$!
  for i in $(seq 1 400); do ip netns exec r-midjd ip link show mx >/dev/null 2>&1 && break; sleep 0.05; done
  ip netns exec r-midjd timeout "$CAP_S" tcpdump -i mx -s "$SNAP" -B 65536 -w "$O/cap-$t.pcap" "tcp port 2096" > "$O/tcpdump-$t.txt" 2>&1
  wait $rp
  echo "$(date +%T) $t done" >> "$O/capture.log"
done

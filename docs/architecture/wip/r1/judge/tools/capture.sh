#!/bin/bash
# capture.sh OUT LABEL... : T14 (i) of the R1 judge. For each binary label
# (BINS file, plus old), one rig run (s5, CELL below) with a packet capture
# of the link's TCP stream inside r-mid (the bottleneck namespace), from the
# moment the namespace exists, for CAP_S seconds; then the TLS record lengths.
# Holds /root/r1-quiet.lock while it runs. Nothing leaves the rig namespaces.
set -u
O=${1:?}; shift
RS=/root/hs2-wt/r1-judge/docs/architecture/wip/rig-scale
CELL=${CELL:-N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20}
CAP_S=${CAP_S:-24}
LOCK=/root/r1-quiet.lock
mkdir -p "$O"
touch "$LOCK"
trap 'rm -f "$LOCK"' EXIT
for b in "$@"; do
  echo "$CELL BIN=$b" > "$O/plan-$b.txt"
  ip netns del r-midjd 2>/dev/null
  REPS=1 RIG_ID=jd BINS=$BINS "$RS/run.sh" "$O/plan-$b.txt" "$O/run-$b" > "$O/run-$b.out" 2>&1 &
  rp=$!
  for i in $(seq 1 200); do ip netns exec r-midjd ip link show mx >/dev/null 2>&1 && break; sleep 0.05; done
  ip netns exec r-midjd timeout "$CAP_S" tcpdump -i mx -s 0 -B 65536 -w "$O/cap-$b.pcap" 'tcp port 2096' > "$O/tcpdump-$b.txt" 2>&1
  wait $rp
  python3 /root/r1-judge/tlsrec.py extract "$O/cap-$b.pcap" "$O/rec-$b.json"
  rm -f "$O/cap-$b.pcap"
done

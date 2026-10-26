#!/bin/bash
# session.sh OUT: the labeled C1 session (R1 task 3, step 3b), run on the exit
# server. flowcap runs on the edge restricted to the test ports 5311/5312 (5 %
# CPU), patgen's server listens there on the edge's hs0 address, and this side
# plays the users through the live tunnel's hs0, one pattern after another,
# PATDUR (90) seconds each with 5 s gaps. Never port 5201.
#   EDGE   ssh target of the edge over the tunnel (root@<edge hs0 address>)
#   EDGEIP the edge's hs0 address
#   LAB    the directory holding patgen and flowcap on both servers
set -u
OUT=${1:?usage: session.sh OUT}
EDGE=${EDGE:?}; EDGEIP=${EDGEIP:?}; LAB=${LAB:-/root/r1-lab}; P=${PATDUR:-90}
mkdir -p "$OUT"
S="ssh -o BatchMode=yes -o ConnectTimeout=20 $EDGE"
$S "systemctl reset-failed r1-flowcap2 r1-patgen 2>/dev/null; mkdir -p /root/r1-flowcap; rm -f /root/r1-flowcap/labeled.jsonl.gz
    systemd-run --quiet --unit=r1-patgen --property=CPUQuota=50% $LAB/patgen server -listen $EDGEIP:5311,$EDGEIP:5312" || exit 1
sleep 1
$S "systemd-run --quiet --unit=r1-flowcap2 --property=CPUQuota=5% --property=CPUQuotaPeriodSec=1s \
      $LAB/flowcap -ports 5311,5312 -dur 12m -status 1m -out /root/r1-flowcap/labeled.jsonl.gz && date +%s%3N" > "$OUT/flowcap_start_ms" || exit 1
sleep 3
: > "$OUT/client.jsonl"
for spec in "bulkdn 5311 1" "bulkup 5311 1" "http 5312 4" "burst 5312 3" "onoff 5312 3" "voip 5312 3"; do
  set -- $spec
  echo "$(date +%T) $1 x$3"
  "$LAB/patgen" client -addr "$EDGEIP:$2" -pat "$1" -dur "${P}s" -n "$3" >> "$OUT/client.jsonl"
  sleep 5
done
sleep 3
$S "systemctl stop r1-flowcap2 r1-patgen; sleep 1; journalctl -u r1-flowcap2 --no-pager -o cat | grep -E 'flowcap' | tail -4" > "$OUT/flowcap.journal"
scp -o BatchMode=yes -q "$EDGE:/root/r1-flowcap/labeled.jsonl.gz" "$OUT/flowcap.jsonl.gz"
echo "done: $(wc -l < "$OUT/client.jsonl") connections"

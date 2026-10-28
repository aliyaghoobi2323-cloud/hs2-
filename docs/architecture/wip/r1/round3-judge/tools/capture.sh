#!/bin/bash
# capture.sh NAME: the judge's DPI captures (R1 round 3; the round-2 judge's
# capture.sh, rig RIG_ID qd): for each line of /root/r3j/plans/NAME.txt (a
# cell with its BIN), one rig run with tcpdump on the bottleneck namespace's
# device mx (both directions, the link connections on port 2096 only), from
# the moment the device is up, CAP_S (24) s, whole packets. Under the rig
# lock (fair FIFO, GAP after the judge's previous block), released at once;
# then (no lock) the records (tlsrec.py extract), the connection profiles
# (dpiconn.py), the record-length comparison with the first capture of
# the s5 cell (tlsrec.py compare), and the pcaps are deleted.
set -u
NAME=${1:?}
RS=/root/r3j/rig; T=/root/hs2-wt/r1-r3j/docs/architecture/wip/r1/round2-judge/tools
O=/root/r3j/$NAME; R=/root/r3j/raw-$NAME
CAP_S=${CAP_S:-24}; GAP=${GAP:-150}
LOCK=/root/r1-quiet.lock
mkdir -p "$O" "$R"
exec 9>/root/r3j/.serial
flock 9
while last=$(cat /root/r3j/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-r3j "judge DPI capture $NAME" > /dev/null || exit 1
t0=$(date +%s)
trap 'rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3j/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3j/locktime.log' EXIT
i=0
grep -v '^\s*\(#\|$\)' /root/r3j/plans/$NAME.txt | tr -d '\r' | while read -r line; do
  i=$((i + 1))
  t=c$i-${line##*BIN=}
  echo "$line" > "$R/plan-$t.txt"
  ip netns del r-midqd 2>/dev/null
  mkdir -p "$R/run-$t"; cp /root/r3j/rigload "$R/run-$t/rigload"
  REPS=1 RIG_ID=qd BINS=/root/r3j/bins.txt OLD_BIN=/root/r3j/bins/hs2-base "$RS/run.sh" "$R/plan-$t.txt" "$R/run-$t" > "$R/run-$t.out" 2>&1 &
  rp=$!
  for k in $(seq 1 400); do ip netns exec r-midqd ip link show mx 2>/dev/null | grep -q ",UP" && break; sleep 0.05; done
  ip netns exec r-midqd timeout "$CAP_S" tcpdump -i mx -s 0 -B 65536 -w "$R/cap-$t.pcap" "tcp port 2096" > "$R/tcpdump-$t.txt" 2>&1
  wait $rp
  echo "$(date -u +%FT%TZ) $t done" >> "$O/capture.log"
done
rm -f "$LOCK"; t1=$(date +%s); echo $t1 > /root/r3j/last-block-end
echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r3j/locktime.log
trap - EXIT
# post-processing, no lock
cd "$R"
for p in cap-*.pcap; do
  t=${p#cap-}; t=${t%.pcap}
  nice python3 "$T/tlsrec.py" extract "$p" "$O/rec-$t.json" >> "$O/extract.log" 2>&1
  nice python3 "$T/dpiconn.py" "$p" "$O/rec-$t.json" --md "$t: $(cat plan-$t.txt)" --sizes "$O/sizes-$t" > "$O/conn-$t.md" 2>&1
done
s5=$(for f in "$O"/rec-*.json; do t=$(basename "$f" .json); t=${t#rec-}; grep -q "MIX=s5" "plan-$t.txt" && echo "$f"; done)
[ -n "$s5" ] && nice python3 "$T/tlsrec.py" compare $s5 > "$O/compare-s5.md" 2>&1
for f in "$O"/*.real_sizes.txt; do
  echo "== $(basename "$f")" >> "$O/dpi_eval2.txt"
  nice python3 "$T/dpi_eval2.py" "$f" 2>&1 | head -4 | tail -2 >> "$O/dpi_eval2.txt"
done
ls -la cap-*.pcap > "$O/pcaps.txt"
rm -f cap-*.pcap
gzip -f "$O"/*.real_sizes.txt 2>/dev/null
echo "$(date -u +%FT%TZ) post done" >> "$O/capture.log"

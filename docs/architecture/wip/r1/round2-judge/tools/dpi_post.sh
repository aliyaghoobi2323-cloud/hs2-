#!/bin/bash
# dpi_post.sh: the round-2 judge's DPI post-processing, after the captures
# (no rig runs then): TLS records of each whole-packet capture
# (tlsrec.py extract), the connection profile of every capture (dpiconn.py,
# each connection's record sizes too), the record-length comparison against
# the base (tlsrec.py compare), obfs/dpi_eval2.py on each binary's records
# and on each connection's; then the pcaps are deleted and
# /root/r2j/dpi.done lets the next blocks start.
set -u
R=/root/r2j/raw-dpi
O=/root/r2j/dpi
T=/root/r2j/tools
mkdir -p "$O"
cd "$R" || exit 1
for p in cap-*.pcap; do
  t=${p#cap-}; t=${t%.pcap}
  cell=$(cat "plan-$t.txt")
  case "$cell" in
    *"RATE=100"*"USERS=7"*) nice python3 "$T/dpiconn.py" "$p" --md "$t: $cell" > "$O/conn-$t.md" 2>&1 ;;
    *)
      nice python3 "$T/tlsrec.py" extract "$p" "$O/rec-$t.json" >> "$O/extract.log" 2>&1
      nice python3 "$T/dpiconn.py" "$p" "$O/rec-$t.json" --md "$t: $cell" --sizes "$O/sizes-$t" > "$O/conn-$t.md" 2>&1 ;;
  esac
done
# record lengths against the base, the s5 25 Mbit/s captures (old first)
s5=$(for b in old b1 b2 b2jx b2bbrq; do ls "$O"/rec-$b-*.json 2>/dev/null | while read f; do grep -q "RATE=25" "$R/plan-$(basename "$f" .json | sed 's/^rec-//').txt" && echo "$f"; done; done)
nice python3 "$T/tlsrec.py" compare $s5 > "$O/compare-s5.md" 2>&1
for f in "$O"/*.real_sizes.txt; do
  echo "== $(basename "$f")" >> "$O/dpi_eval2.txt"
  nice python3 "$T/dpi_eval2.py" "$f" 2>&1 | head -4 | tail -2 >> "$O/dpi_eval2.txt"
done
nice python3 "$T/dpi_eval.py" > "$O/dpi_eval.txt" 2>&1
ls -la "$R"/cap-*.pcap > "$O/pcaps.txt"
rm -f "$R"/cap-*.pcap
touch /root/r2j/dpi.done
echo "$(date +%T) dpi_post done" >> /root/r2j/chain.log

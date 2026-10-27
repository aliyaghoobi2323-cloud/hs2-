#!/bin/bash
# after3.sh: once next2.sh logged "F4 done", 200 s later, the DPI captures
# (capture.sh takes the lock), then the TLS records and the connection
# profile of each, the record-length comparison against the base, and the
# pcaps deleted.
set -u
until grep -q "F4 done" /root/r3a/next.log 2>/dev/null; do sleep 20; done
sleep 200
R=/root/r3a/raw-dpi; mkdir -p $R
/root/r3a/tools/capture.sh $R "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 old b2nx g1e
/root/r3a/tools/capture.sh $R "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" 0 b2nx g1e
echo "$(date -u +%FT%TZ) captures done" >> /root/r3a/next.log
O=/root/r3a/dpi; mkdir -p $O; cd $R
for p in cap-*.pcap; do t=${p#cap-}; t=${t%.pcap}
  nice python3 /root/r3a/tools/tlsrec.py extract $p $O/rec-$t.json >> $O/extract.log 2>&1
  nice python3 /root/r3a/tools/dpiconn.py $p $O/rec-$t.json --md "$t: $(cat plan-$t.txt)" --sizes $O/sizes-$t > $O/conn-$t.md 2>&1
done
s5=$(for b in old b2nx g1e; do ls $O/rec-$b-*.json 2>/dev/null | while read f; do grep -q "RATE=25" "$R/plan-$(basename "$f" .json | sed 's/^rec-//').txt" && echo "$f"; done; done)
nice python3 /root/r3a/tools/tlsrec.py compare $s5 > $O/compare-s5.md 2>&1
for f in $O/*.real_sizes.txt; do
  echo "== $(basename "$f")" >> $O/dpi_eval2.txt
  (cd /root/r3a/tools && nice python3 dpi_eval2.py "$f" 2>&1 | head -4 | tail -2) >> $O/dpi_eval2.txt
done
ls -la $R/cap-*.pcap > $O/pcaps.txt
rm -f $R/cap-*.pcap
echo "$(date -u +%FT%TZ) dpi post done" >> /root/r3a/next.log

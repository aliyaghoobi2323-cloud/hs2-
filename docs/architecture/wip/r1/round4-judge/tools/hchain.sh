#!/bin/bash
# hchain.sh: the second regression (fin2) and the remaining blocks: each
# block once more if cut at its limit; then the DPI captures and the C2 cells
for n in H1 H2 H3 H4 H5 G4 G7 G8; do
  for k in 1 2; do
    echo "$(date +%T) $n start ($k)" >> /root/r4j/chain.log
    /root/r4j/tools/block.sh "$n" "/root/r4j/plans/$n.txt" 3
    echo "$(date +%T) $n done ($?)" >> /root/r4j/chain.log
    want=$(grep -vc "^#" /root/r4j/plans/$n.txt); have=$(wc -l < /root/r4j/raw-$n/res.jsonl 2>/dev/null || echo 0)
    [ "$have" -ge $((want * 3)) ] && break
  done
done
echo "$(date +%T) DPIr start" >> /root/r4j/chain.log
/root/r4j/tools/capture.sh DPIr
echo "$(date +%T) DPIr done ($?)" >> /root/r4j/chain.log
/root/r4j/tools/chain.sh "S6 2"

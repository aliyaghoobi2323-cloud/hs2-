#!/bin/bash
# gchain.sh: the judge regression blocks (3 reps, prod / j3g / c4 / fin), a
# block cut at its limit is run once more (run.sh resumes it), then the DPI
# captures and the C2 loss cells
for n in ${GBLOCKS:-G9 G10 G1 G2 G5 G3 G4 G6 G7 G8}; do
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

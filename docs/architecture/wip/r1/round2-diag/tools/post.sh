#!/bin/bash
# post.sh: after the block — decompose every captured run (decomp.py, 6 at a
# time), then the Q1/Q2, Q3 and Q4 tables. Lab only (round 2, step 1).
set -u
O=${O:-/root/r1-diag/out}
W=/root/r1-diag-work
ls -d $O/q12-*/raw/*/ $O/x-*/raw/*/ $O/x2-*/raw/*/ $O/smoke*/raw/*/ 2>/dev/null | while read -r d; do
  [ -f "$d/e0.pcap" ] && [ -f "$d/probes.json" ] && echo "$d"
done | xargs -P 6 -I{} sh -c 'nice -n 10 python3 '"$W"'/decomp.py {} > {}/decomp.txt 2>&1 || echo "decomp failed: {}"'
python3 $W/q12sum.py $O q12-fq q12-pf x-fq x-pf x2-fq x2-pf > $O/q12sum.md 2> $O/q12sum.err
python3 $W/q3sum.py $O q3-1 q3-2 > $O/q3sum.txt 2> $O/q3sum.err
python3 $W/q4sum.py $O q3-2 q4-solo smoke1 smoke2 > $O/q4sum.txt 2> $O/q4sum.err
echo post done

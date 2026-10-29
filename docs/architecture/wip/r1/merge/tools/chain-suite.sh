#!/bin/bash
# chain-suite.sh: the four locked suite blocks of the R1 merge, one after the other
T=/root/rmerge/tools
L=/root/rmerge/chain-suite.log
echo "start $(date -u +%FT%TZ) pid $$" >> $L
$T/lockrun.sh check "R1 merge step 5: build + make check" 1500 bash -c "$T/blocks.sh build; $T/blocks.sh check" >> $L 2>&1
$T/lockrun.sh race386 "R1 merge step 5: race + 386" 1500 $T/blocks.sh race386 >> $L 2>&1
$T/lockrun.sh fuzzkernel "R1 merge step 5: fuzz + kernel" 1500 $T/blocks.sh fuzzkernel >> $L 2>&1
$T/lockrun.sh arm7 "R1 merge step 5: arm7 + flaky" 1500 $T/blocks.sh arm7 >> $L 2>&1
echo "end $(date -u +%FT%TZ)" >> $L

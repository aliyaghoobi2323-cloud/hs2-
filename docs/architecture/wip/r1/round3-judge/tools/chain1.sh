#!/bin/bash
# chain1.sh: the judge final matrix after F1/F2 (each block waits its turn and the gap)
cd /root/r3j
tools/xblock.sh X1 plans/X.txt /root/r3j/bins/hs2-base /root/r3j/bins/hs2-j3f
tools/block.sh F3 plans/F3.txt 2
tools/block.sh F3 plans/F3.txt 2
tools/block.sh F4 plans/F4.txt 2
tools/block.sh F4 plans/F4.txt 2
tools/block.sh F5 plans/F5.txt 2
tools/block.sh F5 plans/F5.txt 2
tools/block.sh F6 plans/F6.txt 2
tools/block.sh F2b plans/F2b.txt 1
tools/xblock.sh X2 plans/X.txt /root/r3j/bins/hs2-prod /root/r3j/bins/hs2-prodfix

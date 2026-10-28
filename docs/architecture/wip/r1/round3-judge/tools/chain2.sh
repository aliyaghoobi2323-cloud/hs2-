#!/bin/bash
# chain2.sh: the judge s fix of F3 (the BULK link s OWD guard): G1 (guard none / resends only against the base), then F7 (the final candidate j3g in F1 to F3 s cells)
cd /root/r3j
tools/block.sh G1 plans/G1.txt 3
tools/block.sh F7 plans/F7.txt 2

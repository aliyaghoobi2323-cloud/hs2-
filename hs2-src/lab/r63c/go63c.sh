#!/bin/bash
# go63c.sh [JOBFILE] (unit r63cab / r63cab2, lab1): 3 workers over JOBFILE (default j63c.txt).
D=/opt/hs2lab/r63c; export RS=${RS:-$D/res}
for w in 0 1 2; do bash $D/seq63c.sh $w 3 ${1:-$D/j63c.txt} & sleep 20; done
wait; echo "$(date +%T) RUNSDONE" >> $RS/seq.log

#!/bin/bash
# q142.sh (unit df142q, lab2): after the A/B (seq142 j142.txt) -> tests (nice, cores 16-19) next to the soak (TT=2100).
R=/opt/hs2lab/df142
while [ ! -e $R/res/ALLDONE.j142.txt ]; do sleep 2; done
[ -e $R/stop ] && exit 0
nice -n 19 taskset -c 16-19 bash $R/tests142.sh > /dev/null 2>&1 < /dev/null &
TT=2100 /opt/hs2lab/labslot.sh --all bash $R/soak142.sh

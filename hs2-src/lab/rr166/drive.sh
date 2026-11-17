#!/bin/bash
# runs rotation 1 (TT 1000, F slot0 / R slot2, trace off) then rotation 2 (TT 500, R slot0 / F slot2, R rescue trace on)
D=/opt/hs2lab/rr166; A=$D/ab
ROT=1 TT=1000 ORDER="F R" RTRACE=0 bash $D/ab.sh
while [ ! -e $A/r1F/DONE ] || [ ! -e $A/r1R/DONE ]; do sleep 10; done
systemctl stop rr166ab-hold1 2>/dev/null; sleep 30
ROT=2 TT=500 ORDER="R F" RTRACE=1 bash $D/ab.sh
while [ ! -e $A/r2F/DONE ] || [ ! -e $A/r2R/DONE ]; do sleep 10; done
systemctl stop rr166ab-hold2 2>/dev/null
echo "all done $(date -u +%FT%TZ)" > $A/ALLDONE

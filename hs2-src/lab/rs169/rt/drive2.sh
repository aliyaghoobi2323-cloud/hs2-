#!/bin/bash
D=/opt/hs2lab/rs169/rt; A=$D/ab2
ROT=1 TT=420 ORDER="I Z" bash $D/ab2.sh
while [ ! -e $A/r1I/DONE ] || [ ! -e $A/r1Z/DONE ]; do sleep 5; done
systemctl stop rt169g-hold1 2>/dev/null; sleep 15
ROT=2 TT=420 ORDER="Z I" bash $D/ab2.sh
while [ ! -e $A/r2I/DONE ] || [ ! -e $A/r2Z/DONE ]; do sleep 5; done
systemctl stop rt169g-hold2 2>/dev/null
echo "all done $(date -u +%FT%TZ)" > $A/ALLDONE

#!/bin/bash
# start61.sh: after the A/B queue (ALLDONE.${1:-j61.txt}), run the rel61 soak on the whole lab (unit rel61soak).
R=/opt/hs2lab/rel61
while [ ! -e $R/res/ALLDONE.${1:-j61.txt} ]; do sleep 10; done
exec /opt/hs2lab/labslot.sh --all bash $R/soak61.sh

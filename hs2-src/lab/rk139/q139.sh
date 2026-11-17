#!/bin/bash
# q139.sh (unit rk139q, lab2): tests (idle host) -> A/B j139.txt -> 30-min soak of rel62k2 edge + rel61h2 exit.
set -u
R=/opt/hs2lab/rk139
bash $R/tests139.sh
[ -e $R/bin.ok ] || exit 1
[ -e $R/stop ] && exit 0
bash $R/seq139.sh $R/j139.txt
[ -e $R/stop ] && exit 0
/opt/hs2lab/labslot.sh --all bash $R/soak139.sh

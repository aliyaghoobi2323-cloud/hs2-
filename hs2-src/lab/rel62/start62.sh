#!/bin/bash
# start62.sh: after the A/B queue (ALLDONE.j62.txt) AND the RES_GUARD decision (soakbin.ok: hs2R62final = the release build),
# run the rel62 soak on the whole lab (unit rel62soak).
R=/opt/hs2lab/rel62
while [ ! -e $R/res/ALLDONE.${1:-j62.txt} ] || [ ! -e $R/soakbin.ok ]; do sleep 10; done
exec /opt/hs2lab/labslot.sh --all env SB=$R/hs2R62final bash $R/soak62.sh

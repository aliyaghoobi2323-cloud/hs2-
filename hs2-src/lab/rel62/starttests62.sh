#!/bin/bash
# starttests62.sh: tests at nice 19 once the soak has started (unit rel62tests).
R=/opt/hs2lab/rel62
while [ ! -e /opt/hs2lab/rel62soak/t0 ]; do sleep 10; done
exec bash $R/tests62.sh

#!/bin/bash
# starttests61.sh: tests at nice 19 once the soak has started (unit rel61tests).
R=/opt/hs2lab/rel61
while [ ! -e /opt/hs2lab/rel61soak/t0 ]; do sleep 10; done
exec bash $R/tests61.sh

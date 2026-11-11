#!/bin/bash
# setup.sh: build /opt/hs2lab/s49 from this directory (copied to /opt/hs2lab/s49)
# and the lab's smuxwin labsrc2 run.sh; GO_TREE = a hs2-src tree (go.mod, go.sum, third_party)
set -e; D=/opt/hs2lab/s49; T=$1
mkdir -p $D/labsrc $D/res
rm -rf $D/labsrc/lab; cp -r /opt/hs2lab/smuxwin/labsrc2/hs2-src/lab $D/labsrc/
python3 $D/patchrun.py $D/labsrc/lab/run.sh
cp $D/tcprelay.py $D/labsrc/lab/
cp $T/go.mod $T/go.sum $D/labsrc/; rm -rf $D/labsrc/third_party; cp -r $T/third_party $D/labsrc/
chmod +x $D/*.sh

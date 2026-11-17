#!/bin/bash
# q63c.sh (unit r63cq, lab2): tests63c.sh on the idle host, then the >= 4 h soak (unit rel63csoak, labslot --all).
set -u
R=/opt/hs2lab/r63c
bash $R/tests63c.sh
[ -e $R/bin.ok ] || exit 1
[ -e $R/stop ] && exit 0
systemd-run --unit=rel63csoak --collect /opt/hs2lab/labslot.sh --all bash $R/soak63c.sh

#!/bin/bash
# hs123: after the j1 workers stop (stop file), start j2
set -u
D=/opt/hs2lab/hs123
while systemctl is-active --quiet hs123w0 || systemctl is-active --quiet hs123w1 || systemctl is-active --quiet hs123w2; do sleep 5; done
rm -f $D/stop
for w in 0 1 2; do systemd-run --unit=hs123v$w --collect bash $D/seq123.sh $w 3 $D/j2.txt; done

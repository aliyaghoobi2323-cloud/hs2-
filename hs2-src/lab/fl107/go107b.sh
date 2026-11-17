#!/bin/bash
# go107b.sh: wait for the fl107 queue (ALLDONE.w0-2), run j107b (3 workers), then the test suite (whole lab)
set -u; F=/opt/hs2lab/fl107
until [ -e $F/res/ALLDONE.w0 ] && [ -e $F/res/ALLDONE.w1 ] && [ -e $F/res/ALLDONE.w2 ]; do sleep 5; done
mkdir -p $F/resb
for w in 0 1 2; do systemd-run --unit=fl107b$w --collect bash $F/seq107b.sh $w 3 $F/j107b.txt; done
sleep 10
until [ -e $F/resb/ALLDONE.w0 ] && [ -e $F/resb/ALLDONE.w1 ] && [ -e $F/resb/ALLDONE.w2 ]; do sleep 5; done
[ -e $F/stop ] || /opt/hs2lab/labslot.sh --all bash $F/tests107.sh

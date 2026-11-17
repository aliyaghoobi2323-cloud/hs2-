#!/bin/bash
# go107.sh JOBFILE: wait for the fl106 queue2 (ALLDONE.w0-2), then run JOBFILE with 3 workers
set -u; F=/opt/hs2lab/fl107; G=/opt/hs2lab/fl106/res
until [ -e $G/ALLDONE.w0 ] && [ -e $G/ALLDONE.w1 ] && [ -e $G/ALLDONE.w2 ]; do sleep 3; done
for w in 0 1 2; do systemd-run --unit=fl107q$w --collect bash $F/seq107.sh $w 3 $1; done

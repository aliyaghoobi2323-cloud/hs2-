#!/bin/bash
# chain5.sh: after chain2 (the flock) and the DPI post-processing (/root/r2j/dpi.done), the validation of version 5 (b2nx):
# G1 (N=1 latency, duplex, FIFO, P10; 3 repeats), G2 (newheavy, tunping
# N=8, loss; 3) with G2b (policer, N=2; 2), G3 (N=64; 2), a gap of 240 s
# after each lock is released.
set -u
LOG=/root/r2j/chain.log
P=/root/r2j/plans
T=/root/r2j/tools
exec 9> /root/r2j/chain.flock
flock 9
until [ -e /root/r2j/dpi.done ]; do sleep 10; done
echo "$(date +%T) chain5 pid $$ start" >> $LOG
sleep 60
TRACE_SS=1 $T/block2.sh G1 $P/G1.txt 3; echo "$(date +%T) G1 exit $?" >> $LOG
sleep 240
TRACE_SS=1 $T/block2.sh G2 $P/G2.txt 3 $P/G2b.txt 2; echo "$(date +%T) G2 exit $?" >> $LOG
sleep 240
$T/block2.sh G3 $P/G1b.txt 2; echo "$(date +%T) G3 exit $?" >> $LOG
echo "$(date +%T) chain5 done" >> $LOG

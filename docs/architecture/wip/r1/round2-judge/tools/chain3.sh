#!/bin/bash
# chain3.sh: after chain2 (the flock), a gap, then block M3 (the FIFO
# tuning b2kx against b2jx and the base, and b2kx on the other N=1 cells).
set -u
LOG=/root/r2j/chain.log
exec 9> /root/r2j/chain.flock
flock 9
echo "$(date +%T) chain3 pid $$ start" >> $LOG
sleep 240
TRACE_SS=1 /root/r2j/tools/block2.sh M3 /root/r2j/plans/M3.txt 3; echo "$(date +%T) M3 exit $?" >> $LOG
echo "$(date +%T) chain3 done" >> $LOG

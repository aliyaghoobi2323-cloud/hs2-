#!/bin/bash
# drv.sh JOBS P — run the job lines (RUNNER NAME DIR CAR FARM VAR) through labslot, P at a time
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/r48; cd $D
job(){ set -- $1; [ -f $D/stop ] && return; R=$1; N=$2; shift
  echo "$(date +%T) start $N" >> $D/drv.log
  LABSLOT_WAIT_MAX=1500 /opt/hs2lab/labslot.sh $D/$R "$@" >> $D/slot.log 2>&1
  echo "$(date +%T) end $N" >> $D/drv.log; }
export -f job; export D
xargs -P ${2:-3} -I{} -d "\n" bash -c "job \"{}\"" < $1
echo "$(date +%T) done $1" >> $D/drv.log; touch $1.done

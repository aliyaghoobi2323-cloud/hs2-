#!/bin/bash
# drv.sh JOBFILE — run "NAME FARM V BIN [XIR...]" lines, 3 slots
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/s46; cd $D; J=$1
job(){ [ -f $D/stop ] && return; set -- $1; echo "$(date +%T) start $1" >> $D/drv.log
  LABSLOT_WAIT_MAX=1500 /opt/hs2lab/labslot.sh $D/run2.sh "$1" "$2" "$3" "$4" "${*:5}" >> $D/slot.log 2>&1
  echo "$(date +%T) end $1" >> $D/drv.log; }
export -f job; export D
xargs -P 3 -I{} -d "\n" bash -c "job \"{}\"" < $J
echo "$(date +%T) $J done" >> $D/drv.log; touch $D/done.$(basename $J)

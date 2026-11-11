#!/bin/bash
# drv.sh JOBS P — job lines "NAME DIR FARM VAR [ENV=..]" through labslot, P at a time (s50)
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/r50; cd $D
job(){ set -- $1; [ -f $D/stop ] && return; N=$1
  echo "$(date +%T) start $N" >> $D/drv.log
  env ${5:-X=1} LABSLOT_WAIT_MAX=2500 /opt/hs2lab/labslot.sh $D/run4.sh $1 $2 $3 $4 >> $D/slot.log 2>&1
  echo "$(date +%T) end $N" >> $D/drv.log; }
export -f job; export D
xargs -P ${2:-3} -I{} -d "\n" bash -c "job \"{}\"" < $1
echo "$(date +%T) done $1" >> $D/drv.log; touch $1.done

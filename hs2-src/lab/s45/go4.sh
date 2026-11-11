#!/bin/bash
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/s45; cd $D
printf "%s\n" "c1-rpy-1 rpy c1" "c1-anb-4 anb c1" "c1-an-4 an c1" "c1-rpy-2 rpy c1" "c1-r70-4 r70 c1" "c1-an-5 an c1" "c1-anb-5 anb c1" "c1-rpy-3 rpy c1" "c1-r70-5 r70 c1" > $D/jobs4.txt
job(){ set -- $1; [ -f $D/stop ] && return; echo "$(date +%T) start $1" >> $D/drv.log
  LABSLOT_WAIT_MAX=1500 /opt/hs2lab/labslot.sh $D/run1.sh $1 $2 $3 >> $D/slot.log 2>&1
  echo "$(date +%T) end $1" >> $D/drv.log; }
export -f job; export D
xargs -P 1 -I{} -d "\n" bash -c "job \"{}\"" < $D/jobs4.txt
echo "$(date +%T) go4 done" >> $D/drv.log; touch $D/done4

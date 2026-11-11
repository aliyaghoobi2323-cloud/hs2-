#!/bin/bash
# s45 demos: announcement switch old (an) vs fixed (anb) binary; reality with a 70 ms target (r70)
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/s45; cd $D
printf "%s\n" "c1-an-1 an c1" "c1-anb-1 anb c1" "c1-r70-1 r70 c1" "c1-anb-2 anb c1" "c1-an-2 an c1" "c1-r70-2 r70 c1" "c1-an-3 an c1" "c1-anb-3 anb c1" "c1-r70-3 r70 c1" > $D/jobs2.txt
job(){ set -- $1; [ -f $D/stop ] && return; echo "$(date +%T) start $1" >> $D/drv.log
  LABSLOT_WAIT_MAX=900 /opt/hs2lab/labslot.sh $D/run1.sh $1 $2 $3 >> $D/slot.log 2>&1
  echo "$(date +%T) end $1" >> $D/drv.log; }
export -f job; export D
xargs -P 2 -I{} -d "\n" bash -c "job \"{}\"" < $D/jobs2.txt
echo "$(date +%T) go2 done" >> $D/drv.log; touch $D/done2

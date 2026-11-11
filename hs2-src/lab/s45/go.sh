#!/bin/bash
# s45 battery: 3 workers; first the profiled pair + a u8 check, then 8 interleaved reps of c1/u8 x reality/tls
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/s45; cd $D
{
echo "P c1p-reality-1 reality c1"; echo "P c1p-tls-1 tls c1"; echo "N u8-reality-1 reality u8"
echo "N u8-tls-1 tls u8"; echo "N c1-tls-1 tls c1"; echo "N c1-reality-1 reality c1"
for rep in 2 3 4 5 6 7 8; do
  if [ $((rep%2)) = 1 ]; then o="reality tls"; else o="tls reality"; fi
  for v in c1 u8; do for a in $o; do echo "N $v-$a-$rep $a $v"; done; done
done
} > $D/jobs.txt
job(){ set -- $1; [ -f $D/stop ] && return; echo "$(date +%T) start $2" >> $D/drv.log
  if [ $1 = P ]; then PPROF=1 LABSLOT_WAIT_MAX=600 /opt/hs2lab/labslot.sh $D/run1.sh $2 $3 $4 >> $D/slot.log 2>&1
  else LABSLOT_WAIT_MAX=600 /opt/hs2lab/labslot.sh $D/run1.sh $2 $3 $4 >> $D/slot.log 2>&1; fi
  echo "$(date +%T) end $2" >> $D/drv.log; }
export -f job; export D
xargs -P 3 -I{} -d "\n" bash -c "job \"{}\"" < $D/jobs.txt
echo "$(date +%T) all done" >> $D/drv.log; touch $D/done

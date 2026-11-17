#!/bin/bash
# seq140.sh JOBFILE: "TAG ARM MODE" lines one by one on the whole lab (labslot --all, lab1).
D=/opt/hs2lab/ec140; mkdir -p $D/res
while read -r t a m x; do [ -z "$t" ] && continue; [ -e $D/stop ] && break
  for try in 1 2; do
    /opt/hs2lab/labslot.sh --all $D/d140.sh $t $a $m
    grep -q '"error"' $D/res/$t/heavy.json 2>/dev/null || break
    echo "$(date +%T) $t retry" >> $D/res/seq.log; sleep 20
  done
  echo "$(date +%T) $t" >> $D/res/seq.log
done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

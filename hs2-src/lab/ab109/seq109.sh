#!/bin/bash
# seq109.sh JOBFILE: "TAG ARM MODE" lines one by one on the whole lab; retry once on iperf3 error/segfault.
set -u
D=/opt/hs2lab/ab109; mkdir -p $D/res
while read -r t a m x; do [ -z "${t:-}" ] && continue; [ -e $D/stop ] && break
  for try in 1 2; do
    /opt/hs2lab/labslot.sh --all bash $D/d109.sh $t $a $m
    if grep -q '"error"' $D/res/$t/heavy.json 2>/dev/null || [ "$(cat $D/res/$t/segv.txt 2>/dev/null || echo 0)" != 0 ]; then
      echo "$(date +%T) $t retry(segv/err)" >> $D/res/seq.log; mv $D/res/$t $D/res/$t.bad$try 2>/dev/null; sleep 10; continue; fi
    break
  done
  echo "$(date +%T) $t" >> $D/res/seq.log
done < $1
echo ALLDONE > $D/res/ALLDONE.$(basename $1)

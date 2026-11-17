#!/bin/bash
# chain151.sh NAME "TAG:ARM ..." — one after another through labslot
n=$1; shift
for x in $1; do IFS=: read -r t a <<< "$x"; echo "$(date +%T) start $t" >> /opt/hs2lab/sd151/$n.log
  /opt/hs2lab/labslot.sh /opt/hs2lab/sd151/d151.sh $t $a >> /opt/hs2lab/sd151/$n.log 2>&1
  echo "$(date +%T) done $t rc=$?" >> /opt/hs2lab/sd151/$n.log; done

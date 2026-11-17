#!/bin/bash
# chain.sh NAME "TAG:ARM:SC ..." — runs one after another through labslot
n=$1; shift
for x in $1; do IFS=: read -r t a s <<< "$x"; echo "$(date +%T) start $t" >> /opt/hs2lab/up148/$n.log
  /opt/hs2lab/labslot.sh /opt/hs2lab/up148/d148b.sh $t $a $s >> /opt/hs2lab/up148/$n.log 2>&1
  echo "$(date +%T) done $t rc=$?" >> /opt/hs2lab/up148/$n.log; done

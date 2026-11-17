#!/bin/bash
# fetch78.sh OUT: pull result.json/meta.txt of dl78 (l2*/l3*/m2*/m3*) and dl77's
# gate-2 rounds (y7/w7, lab2) into OUT/lab2 and OUT/lab3
O=$1; K=~/.ssh/hs2_claude_ed25519; mkdir -p $O/lab2 $O/lab3
ssh -i $K root@198.51.100.234 'cd /opt/hs2lab/dl76/res && tar cf - $(ls -d l2* m2* n2* p2* y7* w7* 2>/dev/null | sed "s#\$#/result.json#;p;s#result.json#meta.txt#")' 2>/dev/null | tar xf - -C $O/lab2
ssh -i $K root@198.51.100.235 'cd /opt/hs2lab/dl76/res && tar cf - $(ls -d l3* m3[du]* n3* p3* 2>/dev/null | sed "s#\$#/result.json#;p;s#result.json#meta.txt#")' 2>/dev/null | tar xf - -C $O/lab3

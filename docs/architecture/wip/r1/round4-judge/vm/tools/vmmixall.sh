#!/bin/bash
# vmmixall.sh: the item-4 functional cells, each only while the rig lock is
# free or held by r1-r4j and /root/r4j/NOBUILD is absent (nice 19, cores 10-11).
set -u
B=/root/r4/bins; V=/root/r4j/vm/hs2-vm
L=/root/r1-quiet.lock
ok() { [ ! -e /root/r4j/NOBUILD ] && { [ ! -e $L ] || head -c 7 $L 2>/dev/null | grep -q "^r1-r4j "; }; }
cells=(
 "A-prod-edge_new-exit 8 $B/hs2-prod $V"
 "B-new-edge_prod-exit 8 $V $B/hs2-prod"
 "C-j3g-edge_new-exit 8 $B/hs2-j3g $V"
 "D-new-edge_j3g-exit 8 $V $B/hs2-j3g"
 "E-c4-edge_new-exit 8 $B/hs2-c4f $V"
 "F-new-edge_c4-exit 8 $V $B/hs2-c4f"
 "G-new-edge-many2_new-exit 8 $V $V HS2_TUNE_QUIET_MANY=2"
 "H-new-edge_new-exit-many2 8 $V $V - HS2_TUNE_QUIET_MANY=2"
 "I-new_new 8 $V $V"
 "J-before-c4-edge_j3g-exit 8 $B/hs2-c4f $B/hs2-j3g"
 "K-new-edge-n2_c4-exit 1 $V $B/hs2-c4f HS2_TUNE_QUIET_N=2"
 "L-new-edge-n2_new-exit 1 $V $V HS2_TUNE_QUIET_N=2"
 "M-before-c4-edge-n2_c4-exit 1 $B/hs2-c4f $B/hs2-c4f HS2_TUNE_QUIET_N=2"
 "N-new-edge_new-exit-n2 1 $V $V - HS2_TUNE_QUIET_N=2"
 "O-switchoff-new-edge-n2_c4-exit 1 $V $B/hs2-c4f HS2_TUNE_QUIET_N=2,HS2_TUNE_QUIET_FOLLOW=0"
)
: > /root/r4j/vm/mixall.txt
for c in "${cells[@]}"; do
	set -- $c
	ee=${5:-}; xe=${6:-}; [ "$ee" = - ] && ee=""; ee=${ee//,/ }; xe=${xe//,/ }
	until ok; do sleep 20; done
	nice -n 19 taskset -c 10,11 /root/r4j/vm/vmmix.sh "$1" "$2" "$3" "$4" "$ee" "$xe" >> /root/r4j/vm/mixall.txt 2>&1
	echo >> /root/r4j/vm/mixall.txt
done
echo ALLDONE >> /root/r4j/vm/mixall.txt

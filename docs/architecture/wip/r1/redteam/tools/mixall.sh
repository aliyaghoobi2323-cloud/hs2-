#!/bin/bash
# mixall.sh — build the red team's binary, then the mixed-version cases, each
# only while no rig lock is held.
set -u
B=/root/r3j/bins; R=/root/rred/bin
mkdir -p $R
/root/rred/redrun.sh build-red /root/hs2-wt/r1-red/hs2-src env CGO_ENABLED=0 /usr/local/go/bin/go build -o $R/hs2-red ./cmd/hs2
cases=(
 "m1 $B/hs2-j3g $B/hs2-base $B/hs2-j3g"
 "m2 $R/hs2-red $B/hs2-base $R/hs2-red"
 "m3 $B/hs2-prod $B/hs2-j3g"
 "m4 $R/hs2-red $B/hs2-prod $R/hs2-red"
 "m5 $B/hs2-base $R/hs2-red"
)
for c in "${cases[@]}"; do
	set -- $c
	for try in 1 2 3 4 5 6; do
		while [ -e /root/r1-quiet.lock ]; do sleep 5; done
		/root/rred/mixrun.sh "$@"; rc=$?
		[ $rc != 3 ] && break
		sleep 10
	done
done
echo done > /root/rred/mixall.done

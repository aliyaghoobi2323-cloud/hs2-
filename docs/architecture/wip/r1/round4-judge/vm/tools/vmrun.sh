#!/bin/bash
# vmrun.sh NAME DIR CMD...: run CMD in DIR (nice 19, cores 10-11, GOMAXPROCS=2)
# only while the rig lock is free or held by r1-r4j and /root/r4j/NOBUILD is
# absent; stops its own process group if someone else takes the lock.
# Output: /root/r4j/vm/NAME.txt (rc in the last line).
set -u
NAME=$1; DIR=$2; shift 2
L=/root/r1-quiet.lock; OUT=/root/r4j/vm/$NAME.txt
ok() { [ ! -e /root/r4j/NOBUILD ] && { [ ! -e $L ] || head -c 7 $L 2>/dev/null | grep -q "^r1-r4j "; }; }
until ok; do sleep 30; done
cd "$DIR"
setsid nice -n 19 taskset -c 10,11 env GOMAXPROCS=2 "$@" > "$OUT" 2>&1 &
pid=$!
while kill -0 $pid 2>/dev/null; do
	if ! ok; then kill -- -$pid 2>/dev/null; sleep 1; kill -9 -- -$pid 2>/dev/null; echo "KILLED: rig lock taken $(date -u +%T)" >> "$OUT"; wait $pid; exit 3; fi
	sleep 2
done
wait $pid; rc=$?
echo "rc=$rc" >> "$OUT"
exit $rc

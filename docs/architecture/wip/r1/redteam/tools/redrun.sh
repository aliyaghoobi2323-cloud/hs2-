#!/bin/bash
# redrun.sh NAME DIR CMD... — run CMD (in DIR) only while no rig lock is held:
# waits for /root/r1-quiet.lock to be gone, runs CMD niced, kills it the
# moment a lock appears (then waits and starts again from scratch).
# Output: /root/rred/NAME.out (last attempt), /root/rred/NAME.rc
set -u
name=$1; dir=$2; shift 2
L=/root/r1-quiet.lock
out=/root/rred/$name.out
rm -f /root/rred/$name.rc
for try in $(seq 1 60); do
	while [ -e $L ]; do sleep 5; done
	echo "== try $try $(date -u +%FT%TZ)" > $out
	( cd $dir && exec setsid nice -n 19 ionice -c3 env GOMAXPROCS=6 "$@" ) >> $out 2>&1 &
	pid=$!
	killed=0
	while kill -0 $pid 2>/dev/null; do
		if [ -e $L ]; then
			kill -TERM -$pid 2>/dev/null; sleep 1; kill -KILL -$pid 2>/dev/null
			killed=1
			echo "== killed: rig lock taken $(date -u +%FT%TZ)" >> $out
			break
		fi
		sleep 1
	done
	wait $pid; rc=$?
	if [ $killed = 0 ]; then echo $rc > /root/rred/$name.rc; echo "== rc $rc $(date -u +%FT%TZ)" >> $out; exit 0; fi
	sleep 10
done
echo gaveup > /root/rred/$name.rc

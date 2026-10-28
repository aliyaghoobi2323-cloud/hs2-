#!/bin/bash
# withmem.sh NAME PLAN REPS: block.sh NAME PLAN REPS with memwatch.sh sampling into /root/r3j/NAME-mem
/root/r3j/tools/memwatch.sh /root/r3j/$1-mem & W=$!
/root/r3j/tools/block.sh "$@"
kill $W; pkill -f "tools/memwatch.sh /root/r3j/$1-mem" 2>/dev/null

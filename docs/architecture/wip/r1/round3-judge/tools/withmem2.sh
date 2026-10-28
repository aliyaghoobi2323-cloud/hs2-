#!/bin/bash
# withmem2.sh NAME PLAN REPS: block.sh with memwatch2.sh sampling into /root/r3j/NAME-mem
/root/r3j/tools/memwatch2.sh /root/r3j/$1-mem & W=$!
/root/r3j/tools/block.sh "$@"
kill $W; pkill -f "tools/memwatch2.sh /root/r3j/$1-mem" 2>/dev/null

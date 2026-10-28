#!/bin/bash
# withsnap.sh NAME PLAN REPS: block.sh with memsnap.sh sampling into /root/r4/NAME-mem
/root/r4/tools/memsnap.sh /root/r4/$1-mem 4 & W=$!
/root/r4/tools/block.sh "$@"
kill $W; pkill -f "tools/memsnap.sh /root/r4/$1-mem" 2>/dev/null

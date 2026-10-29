#!/bin/bash
# after.sh PID ARGS...: chain.sh ARGS once process PID has ended (block order)
p=$1; shift
while kill -0 $p 2>/dev/null; do sleep 10; done
exec /root/r4j/tools/chain.sh "$@"

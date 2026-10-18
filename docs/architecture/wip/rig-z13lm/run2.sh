#!/bin/bash
# run2.sh EDGEBIN EXITBIN EXITOUT [env...]: one TestZ5LM run split in two processes of one network
# namespace (production "high" TCP buffers): the exit (Z5_LM_ROLE=exit: RunKharej and the panel)
# from EXITBIN, its output to EXITOUT, and the edge (emulator, LinkManager, users) from EDGEBIN.
R=$(cd "$(dirname "$0")" && pwd)
E=$1; X=$2; XO=$3; shift 3
exec unshare -n env "$@" Z5E="$R/bin/$E.test" Z5X="$R/bin/$X.test" Z5XO="$XO" bash -c '
echo "4096 131072 33554432" > /proc/sys/net/ipv4/tcp_rmem
echo "4096 65536 33554432" > /proc/sys/net/ipv4/tcp_wmem
st=$Z5XO.stop; rm -f $st
Z5_LM_ROLE=exit Z5_LM_STOP=$st timeout ${Z5_TIMEOUT:-500} $Z5X -test.run "^TestZ5LM$" -test.v > $Z5XO 2>&1 &
xp=$!
sleep 2
Z5_LM_ROLE=edge timeout ${Z5_TIMEOUT:-500} $Z5E -test.run "^TestZ5LM$" -test.v
touch $st; wait $xp; rm -f $st'

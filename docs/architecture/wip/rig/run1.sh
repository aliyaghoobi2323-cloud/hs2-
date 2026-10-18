#!/bin/bash
# run1.sh BIN [env...]: one rig run in its own network namespace with the
# production "high" profile TCP buffers (tune.Build: tcp_rmem/tcp_wmem max 32 MiB)
R=$(cd "$(dirname "$0")" && pwd)
BIN=$1; shift
exec unshare -n env "$@" bash -c '
echo "4096 131072 33554432" > /proc/sys/net/ipv4/tcp_rmem
echo "4096 65536 33554432" > /proc/sys/net/ipv4/tcp_wmem
exec timeout ${Z5_TIMEOUT:-300} '"$R/bin/$BIN.test"' -test.run "^${Z5_TEST:-TestZ5Rig}$" -test.v' 

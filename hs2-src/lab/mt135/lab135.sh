#!/bin/bash
# lab135.sh MODE TAG  (mt135, lab1 only): 3 paths, l3mtcp, MODE rev|dir.
#  path a 192.168.50.x: clean.  path b: IPv6 fd00:51:: in rev (IPv4 192.168.51.x in dir; the harness has no v6 in direct)
#  -> at +40 s a large-packet BLACKHOLE (packets > 1300 bytes dropped at the mid ingress, endpoint gso off, no ICMP), mtu_probing=1 both sides.
#  path c 192.168.52.x -> at +40 s a route MTU 1300 bottleneck on both ends (the PMTU state ICMP frag-needed sets).
#  Snapshots of both status files + `hs2 lb show -status` at +35 (before), +65, +95.
set -u
RUNMODE=$1; TAG=$2; F=/opt/hs2lab/mt135; L=/opt/hs2lab/fl106/labsrc/lab
O=$F/res/$TAG; rm -rf $O; mkdir -p $O
W=/tmp/l135-$TAG; BIN=$F/bin/hs2
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod
export MODE=l3mtcp EMU=kernel RATE=300mbit DELAY=40ms QUEUE=100ms PATHS=3 LB_MODE=${LB_MODE:-auto} REVERSE=1 NOWARM=1 WORKDIR=$W
export BULK=4 CALL=2mbit GAME=1 CROWD=200 MIN_LINKS=12 MAX_LINKS=24 T=110s LOSS=0 KILL_AT=0 PPROF=0
export KHAREJ_EXTRA="\"max_links\":24,\"min_links\":12,"
export SPTBIN=/opt/hs2lab/cg86/spt
if [ "$RUNMODE" = dir ]; then export LB_DIRECT=1 LB_V6=0; B4=1; else export LB_DIRECT=0 LB_V6=1; B4=0; fi
SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
(
  for i in $(seq 1 120); do [ -s "$SF" ] && break; sleep 1; done
  ip netns exec ir sysctl -qw net.ipv4.tcp_mtu_probing=1; ip netns exec kh sysctl -qw net.ipv4.tcp_mtu_probing=1
  snap(){ cp "$SF" $O/ir-$1.json; cp "$SFK" $O/kh-$1.json; date +%T > $O/t-$1.txt
          $BIN lb show -status "$SF" > $O/lbshow-ir-$1.txt 2>&1; $BIN lb show -status "$SFK" > $O/lbshow-kh-$1.txt 2>&1; }
  sleep 35; snap a
  sleep 5; echo "$(date +%T) events" > $O/events.txt
  # path c: route MTU 1300 both ends (+ the policy tables the harness uses)
  ip -n kh route replace 192.168.52.1/32 dev vkc mtu lock 1300
  ip -n ir route replace 192.168.52.2/32 dev vic mtu lock 1300
  ip -n kh route replace 192.168.50.1/32 dev vkc src 192.168.52.2 table 152 mtu lock 1300 2>/dev/null
  ip -n ir route replace 192.168.50.2/32 dev vic src 192.168.52.1 table 152 mtu lock 1300 2>/dev/null
  # path b: blackhole for packets over 1300 bytes, no ICMP
  #   (veth forwarding does not enforce the mid MTU: drop by length at the mid ingress, before its mirred)
  ip netns exec ir ethtool -K vib gso off >/dev/null 2>&1; ip netns exec kh ethtool -K vkb gso off >/dev/null 2>&1
  for d in xab xbb; do
    tc -n mid filter add dev $d ingress prio 1 protocol ipv6 basic match 'cmp(u16 at 4 layer network gt 1260)' action drop
    tc -n mid filter add dev $d ingress prio 2 protocol ip basic match 'cmp(u16 at 2 layer network gt 1300)' action drop
  done
  ip -n kh route show 192.168.52.1 >> $O/events.txt; tc -n mid filter show dev xbb ingress >> $O/events.txt 2>&1
  sleep 25; snap b
  sleep 30; snap c
  ip netns exec ir ss -tni > $O/ss-ir.txt 2>&1; ip netns exec kh ss -tni > $O/ss-kh.txt 2>&1
  tc -n mid -s filter show dev xbb ingress > $O/tc-xbb.txt 2>&1; tc -n mid -s filter show dev xab ingress > $O/tc-xab.txt 2>&1
  timeout 60 $BIN doctor > $O/doctor.txt 2>&1
) & EVP=$!
env BIN=$BIN BIN_KH=$BIN IR_ENV="HS2_LB_PMTU_HINT=1" KH_ENV="HS2_LB_PMTU_HINT=1" LOGDIR=$O RUNTAG=$TAG bash $L/run117.sh > $O/run.out 2> $O/run.err
wait $EVP 2>/dev/null
grep -h "lb pmtu" $O/*.log > $O/pmtu-log.txt 2>/dev/null
rm -rf "$W"
echo DONE > $O/DONE

#!/bin/bash
# rig.sh: the network of the big rig (R1 task 2). Three network namespaces,
# r-edge, r-mid and r-exit (with the suffix $RIG_ID, so rigs can run side by
# side), the bottleneck in r-mid and the users' source addresses in r-edge.
# It never touches the root namespace: every device is made inside r-mid.
#
#   RATE=50 RTT=72 LOSS=0 MODE=fqcodel NUSERS=200 V6=1 NTUN=4 rig.sh up
#   rig.sh tun-up      (once hs2 has made hs0 on both ends: IPv6 on hs0, routes back to the TUN users)
#   rig.sh down
#   rig.sh police      (loop: one policer per hs2 link connection, MODE with police:PR)
#   rig.sh show        (tc -s of the bottleneck, both directions)
#   rig.sh wait-links N SECONDS   (until r-exit has N established link sockets)
#
# Path (MTU 1500 everywhere, offloads off, so the bottleneck sees real packets):
#   r-edge e0 10.201.0.1/24 fd00:201::1/64
#     <-> r-mid me 10.201.0.254 | mx 10.202.0.254
#     <-> r-exit x0 10.202.0.1/24 fd00:202::1/64
# hs2 links: the exit dials the edge's 10.201.0.1:2096 (reverse tunnel).
# Each direction leaves r-mid through one egress (me: download, exit to edge;
# mx: upload, edge to exit), with the same tree on both:
#   root netem delay RTT/2 [loss LOSS%] (limit 1e6: the queue is the leaf's)
#     -> htb, one class rate RATE Mbit/s (the bottleneck; RATE_UP, default
#        RATE, for the upload direction mx: an asymmetric path)
#        -> leaf by MODE:
#           fqcodel     fq_codel (defaults: target 5ms, interval 100ms, limit 10240)
#           pfifo:L     pfifo limit L packets (drop-tail)
#           agg:AR      tbf rate AR Mbit/s, one shaper for all links, with a
#                       drop-tail buffer of AGGQ_MS ms (default 50) at AR
#           police:PR   fq_codel, plus a policer of PR Mbit/s on each hs2 link
#                       connection (flower on its five-tuple at r-mid's ingress,
#                       both directions; `rig.sh police` adds them as links appear)
#           police:PR+LEAF   the policer and that leaf together, e.g.
#                       police:20+agg:300: the mixed regime (per-connection
#                       policer and an aggregate shaper at once)
# Endpoint qdiscs as on the field servers: fq on the edge's e0, fq_codel on the
# exit's x0. TCP sysctls in r-edge and r-exit: the "high" profile hs2 applies
# on these hosts (tune.Build), only the per-namespace ones.
# Users (r-edge): NUSERS IPv4 addresses 10.100.a.b/32 on lo (ip addr add, at
# most 500), the i-th (from 0) is 10.100.(i/250).(i%250+1); V6 other than 0
# adds the /64 fd77:1::/64 as a local route with ip_nonlocal_bind, so a user
# can rotate over any address in it (one /64 = one user in R1).
# TUN users (r-edge): NTUN inner addresses on lo, the i-th (from 0)
# 10.110.(i/250).(i%250+1)/32 and fd00:110:(i+1)::1/128 (each in a /64 of its
# own). Their packets leave through hs0 (routes to the exit's hs0 address);
# `tun-up` gives hs0 fd00:77::1/64 (edge) and fd00:77::2/64 (exit) and routes
# 10.110.0.0/16 and fd00:110::/32 back through the exit's hs0.
set -u
ID=${RIG_ID:-}
E=r-edge$ID; M=r-mid$ID; X=r-exit$ID
cmd=${1:-}

q() { "$@" >/dev/null 2>&1; }
die() { echo "rig.sh: $*" >&2; exit 1; }

nsx() { local n=$1; shift; ip netns exec "$n" "$@"; }

# MODE: POL (policer Mbit/s, empty: none) and LEAF
parse_mode() {
  local p
  POL=""; LEAF=""
  IFS=+ read -ra parts <<< "${MODE:-fqcodel}"
  for p in "${parts[@]}"; do
    case "$p" in
      police:*) [ -z "$POL" ] || die "two policers in MODE $MODE"; POL=${p#police:} ;;
      fqcodel|pfifo:*|agg:*) [ -z "$LEAF" ] || die "two leaves in MODE $MODE"; LEAF=$p ;;
      *) die "unknown MODE part $p" ;;
    esac
  done
  [ -n "$LEAF" ] || LEAF=fqcodel
}

tcp_sysctls() {
  local n=$1
  nsx $n sysctl -qw net.ipv4.tcp_congestion_control=bbr \
    "net.ipv4.tcp_rmem=4096 131072 33554432" "net.ipv4.tcp_wmem=4096 65536 33554432" \
    net.ipv4.tcp_notsent_lowat=131072 net.ipv4.tcp_slow_start_after_idle=0 \
    net.ipv4.tcp_mtu_probing=1 net.ipv4.tcp_fin_timeout=20 net.ipv4.tcp_tw_reuse=1 \
    net.core.somaxconn=4096 net.ipv4.tcp_max_syn_backlog=4096 \
    "net.ipv4.ip_local_port_range=10000 60999" || die "sysctl in $n"
}

offloads_off() { q nsx $1 ethtool -K $2 tso off gso off gro off tx off rx off; }

# bottleneck DEV: the tree above on r-mid's egress DEV
bottleneck() {
  local d=$1 rate=$2 half loss leaf burst
  half=$(awk -v r="$RTT" 'BEGIN{printf "%.3f", r/2}')
  loss=""
  awk -v l="$LOSS" 'BEGIN{exit !(l>0)}' && loss="loss ${LOSS}%"
  nsx $M tc qdisc add dev $d root handle 1: netem delay ${half}ms $loss limit 1000000 || die "netem on $d"
  # htb burst: 1 ms at the rate, at least 16 KB (smaller bursts cannot hold
  # the rate at 1 Gbit/s with 1 ms timer slack)
  burst=$(awk -v r="$rate" 'BEGIN{b=r*1e6/8*0.001; if(b<16000)b=16000; printf "%d", b}')
  nsx $M tc qdisc add dev $d parent 1:1 handle 2: htb default 10 || die "htb on $d"
  nsx $M tc class add dev $d parent 2: classid 2:10 htb rate ${rate}mbit ceil ${rate}mbit \
    burst $burst cburst $burst quantum 60000 || die "htb class on $d"
  case "$LEAF" in
    fqcodel) leaf="fq_codel" ;;
    pfifo:*) leaf="pfifo limit ${LEAF#pfifo:}" ;;
    agg:*)
      local ar=${LEAF#agg:} aq=${AGGQ_MS:-50} tb
      tb=$(awk -v r="$ar" 'BEGIN{b=r*1e6/8*0.001; if(b<16000)b=16000; printf "%d", b}')
      leaf="tbf rate ${ar}mbit burst $tb latency ${aq}ms" ;;
  esac
  nsx $M tc qdisc add dev $d parent 2:10 handle 3: $leaf || die "leaf $leaf on $d"
}

up() {
  : ${RATE:?} ${RTT:?} ${LOSS:=0} ${MODE:=fqcodel} ${NUSERS:=1} ${V6:=0} ${NTUN:=0}
  [ "$NUSERS" -le 500 ] || die "NUSERS $NUSERS > 500"
  [ "$NTUN" -le 500 ] || die "NTUN $NTUN > 500"
  parse_mode
  down 2>/dev/null
  for n in $E $M $X; do ip netns add $n || die "netns add $n"; done
  for n in $E $M $X; do ip -n $n link set lo up; done
  # both pairs are made inside r-mid, then one end moves to its namespace
  ip -n $M link add me type veth peer name e0 netns $E || die "veth edge"
  ip -n $M link add mx type veth peer name x0 netns $X || die "veth exit"
  ip -n $E addr add 10.201.0.1/24 dev e0
  ip -n $E addr add fd00:201::1/64 dev e0 nodad
  ip -n $M addr add 10.201.0.254/24 dev me
  ip -n $M addr add fd00:201::fe/64 dev me nodad
  ip -n $M addr add 10.202.0.254/24 dev mx
  ip -n $M addr add fd00:202::fe/64 dev mx nodad
  ip -n $X addr add 10.202.0.1/24 dev x0
  ip -n $X addr add fd00:202::1/64 dev x0 nodad
  for x in "$E e0" "$M me" "$M mx" "$X x0"; do
    set -- $x
    offloads_off $1 $2
    ip -n $1 link set dev $2 up
  done
  ip -n $E route add default via 10.201.0.254
  ip -n $E -6 route add default via fd00:201::fe
  ip -n $X route add default via 10.202.0.254
  ip -n $X -6 route add default via fd00:202::fe
  nsx $M sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1 || die "forwarding"
  tcp_sysctls $E; tcp_sysctls $X
  nsx $E tc qdisc replace dev e0 root fq || die "fq on e0"
  nsx $X tc qdisc replace dev x0 root fq_codel || die "fq_codel on x0"
  bottleneck me "$RATE"; bottleneck mx "${RATE_UP:-$RATE}"
  if [ -n "$POL" ]; then
    for d in me mx; do nsx $M tc qdisc add dev $d clsact || die "clsact $d"; done
  fi
  # users, and the TUN users' inner addresses
  {
    for i in $(seq 0 $((NUSERS - 1))); do
      echo "addr add 10.100.$((i / 250)).$((i % 250 + 1))/32 dev lo"
    done
    for i in $(seq 0 $((NTUN - 1))); do
      echo "addr add 10.110.$((i / 250)).$((i % 250 + 1))/32 dev lo"
      printf 'addr add fd00:110:%x::1/128 dev lo nodad\n' $((i + 1))
    done
  } | ip -n $E -batch - || die "user addresses"
  if [ "$V6" != 0 ]; then
    ip -n $E -6 route add local fd77:1::/64 dev lo table local || die "v6 /64"
    nsx $E sysctl -qw net.ipv6.ip_nonlocal_bind=1 || die "v6 nonlocal bind"
  fi
  # the path answers before anyone uses it
  nsx $E ping -c1 -i0.5 -w6 10.202.0.1 >/dev/null || die "edge cannot reach exit"
}

# tun-up: IPv6 on hs0 (both ends) and the routes back to the TUN users; hs2
# makes hs0 at its start, so this runs once both hs2 are up
tun_up() {
  local n
  for n in $E $X; do ip -n $n link show hs0 >/dev/null 2>&1 || die "no hs0 in $n"; done
  ip -n $E addr replace fd00:77::1/64 dev hs0 nodad || die "hs0 v6 edge"
  ip -n $X addr replace fd00:77::2/64 dev hs0 nodad || die "hs0 v6 exit"
  ip -n $X route replace 10.110.0.0/16 dev hs0 || die "route to TUN users"
  ip -n $X -6 route replace fd00:110::/32 dev hs0 || die "v6 route to TUN users"
  # the first packets can come before the link carries TUN frames (its TLS
  # handshake takes a few RTTs after the socket is up): retry for up to 10 s
  nsx $E ping -c1 -i0.5 -w10 10.77.0.2 >/dev/null || die "no answer through hs0"
}

down() {
  local p
  for n in $E $M $X; do
    # nothing may outlive the rig: kill what still runs in its namespaces
    for p in $(ip netns pids $n 2>/dev/null); do kill $p 2>/dev/null; done
  done
  sleep 0.2
  for n in $E $M $X; do
    for p in $(ip netns pids $n 2>/dev/null); do kill -9 $p 2>/dev/null; done
    ip netns del $n 2>/dev/null
  done
  return 0
}

# police: every 0.5 s, a flower filter with a policer for each established
# link connection not yet policed, in both directions (r-mid ingress: mx gets
# the exit's packets, me the edge's). Burst POLICE_BURST_MS (default 20) ms of
# PR, at least 16 KB. Prints each connection it polices.
police() {
  local burst seen=" " p
  parse_mode
  [ -n "$POL" ] || die "MODE $MODE has no police:PR"
  burst=$(awk -v r="$POL" -v m="${POLICE_BURST_MS:-20}" 'BEGIN{b=r*1e6/8*m/1000; if(b<16000)b=16000; printf "%d", b}')
  while true; do
    for p in $(nsx $X ss -HtnO state established '( dport = :2096 )' 2>/dev/null | awk '{split($3,a,":"); print a[length(a)]}'); do
      case "$seen" in *" $p "*) continue ;; esac
      nsx $M tc filter add dev mx ingress protocol ip prio 10 flower ip_proto tcp \
        src_ip 10.202.0.1 src_port $p dst_port 2096 \
        action police rate ${POL}mbit burst $burst conform-exceed drop/ok >/dev/null 2>&1 &&
      nsx $M tc filter add dev me ingress protocol ip prio 10 flower ip_proto tcp \
        dst_ip 10.202.0.1 dst_port $p src_port 2096 \
        action police rate ${POL}mbit burst $burst conform-exceed drop/ok >/dev/null 2>&1 &&
        echo "$(date +%s.%N) policed $p"
      seen="$seen$p "
    done
    sleep 0.5
  done
}

show() {
  for d in me mx; do
    echo "== $d"; nsx $M tc -s qdisc show dev $d
    nsx $M tc -s filter show dev $d ingress 2>/dev/null | grep -c "police" | sed 's/^/policers: /'
  done
}

# wait-links N SECONDS: the exit's established link sockets reach N
wait_links() {
  local n=$1 t=$2 i c=0
  for i in $(seq 1 $((t * 4))); do
    c=$(nsx $X ss -Htn state established '( dport = :2096 )' 2>/dev/null | wc -l)
    [ "$c" -ge "$n" ] && { echo $c; return 0; }
    sleep 0.25
  done
  echo $c; return 1
}

# qpolice PR: once, a policer of PR Mbit/s on each QUIET link connection up
# now (its socket's congestion control is not bbr), both directions, at
# r-mid's ingress — a DPI box that throttles the flows it flagged, by
# five-tuple; a connection that comes up later is not policed (R1 round 5:
# the engine review's B2, a QUIET link alive but slow). Prints each one.
qpolice() {
  local pr=${1:?} burst p
  burst=$(awk -v r="$pr" -v m="${POLICE_BURST_MS:-20}" 'BEGIN{b=r*1e6/8*m/1000; if(b<16000)b=16000; printf "%d", b}')
  for d in me mx; do nsx $M tc qdisc add dev $d clsact 2>/dev/null; done
  for p in $(nsx $X ss -HtinO state established '( dport = :2096 )' 2>/dev/null | awk '$0 !~ / bbr / {split($4,a,":"); print a[length(a)]}'); do
    nsx $M tc filter add dev mx ingress protocol ip prio 9 flower ip_proto tcp \
      src_ip 10.202.0.1 src_port $p dst_port 2096 \
      action police rate ${pr}mbit burst $burst conform-exceed drop/ok >/dev/null 2>&1 &&
    nsx $M tc filter add dev me ingress protocol ip prio 9 flower ip_proto tcp \
      dst_ip 10.202.0.1 dst_port $p src_port 2096 \
      action police rate ${pr}mbit burst $burst conform-exceed drop/ok >/dev/null 2>&1 &&
      echo "$(date +%s.%N) policed QUIET $p at ${pr} Mbit/s"
  done
}

case "$cmd" in
  up) up ;;
  qpolice) qpolice "$2" ;;
  tun-up) tun_up ;;
  down) down ;;
  police) police ;;
  show) show ;;
  wait-links) wait_links "$2" "$3" ;;
  *) echo "usage: rig.sh up|tun-up|down|police|qpolice PR|show|wait-links N S" >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# End-to-end gen-2 datagram TUN lab: iran <-> [netem] <-> kharej in network
# namespaces, real hs2 dgtun on each side (a routed TUN over a POOL of datagram
# carriers), over a chosen encapsulation. Proves:
#   - packets really go through the TUN (ping 10.77.0.1 -> 10.77.0.2),
#   - a forwarded user port reaches the panel over the tun (probe throughput),
#   - the pool comes up on udp/icmp/gre/ipip/ipx alike.
# Prints one JSON line. Needs root, iproute2, ethtool.
#
#   ENCAP=icmp RATE=50mbit DELAY=20ms LOSS=0.01 lab/dgtun.sh
#
# Env [defaults]: ENCAP [udp] PROTO [0] REVERSE [0] RATE [50mbit] DELAY [20ms]
#   QUEUE [300ms] LOSS [0] BURSTLOSS [0] BADMS [0] FLOWRATE [0] ALLOW [""]
#   BULK [4] UP [0] T [12s] MIN [2] MAX [8] KEEP [0] LAB_ID [""]
#   NETEM_EXTRA [""]: more netem flags, e.g. the per-destination policer:
#     NETEM_EXTRA="-dstpolice 60mbit -dstproto 1"
set -u
D=$(cd "$(dirname "$0")" && pwd)
ID=${LAB_ID:-}
W=${WORKDIR:-/tmp/hs2dgtun}$ID
IR=dir$ID; KH=dkh$ID; MID=dmid$ID
VI=dvi$ID; VK=dvk$ID; MA=dxa$ID; MB=dxb$ID
BIN=${BIN:-}; ENCAP=${ENCAP:-udp}; PROTO=${PROTO:-0}; REVERSE=${REVERSE:-0}
RATE=${RATE:-50mbit}; DELAY=${DELAY:-20ms}; QUEUE=${QUEUE:-300ms}; LOSS=${LOSS:-0}
BURSTLOSS=${BURSTLOSS:-0}; BADMS=${BADMS:-0}; FLOWRATE=${FLOWRATE:-0}; ALLOW=${ALLOW:-}
BULK=${BULK:-4}; UP=${UP:-0}; SEG=${SEG:-0}; T=${T:-12s}; MIN=${MIN:-2}; MAX=${MAX:-8}

cleanup(){
  for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in $(cat "$W"/pids 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  ip netns del $IR 2>/dev/null; ip netns del $KH 2>/dev/null; ip netns del $MID 2>/dev/null
}
mkdir -p "$W"; cleanup; rm -f "$W"/pids "$W"/*.log
[ -x "$W/netem" ] || (cd "$D/.." && go build -o "$W/netem" ./lab/netem) || exit 1
[ -x "$W/probe" ] || (cd "$D/.." && go build -o "$W/probe" ./lab/probe) || exit 1
if [ -z "$BIN" ]; then
  (cd "$D/.." && CGO_ENABLED=0 go build -o "$W/hs2" ./cmd/hs2) || exit 1
  BIN="$W/hs2"
fi

ip netns add $IR; ip netns add $KH; ip netns add $MID
ip link add $VI netns $IR type veth peer name $MA netns $MID
ip link add $VK netns $KH type veth peer name $MB netns $MID
ip -n $IR addr add 192.168.50.1/24 dev $VI
ip -n $KH addr add 192.168.50.2/24 dev $VK
for n in $IR $KH $MID; do ip -n $n link set lo up; done
for x in "$IR $VI" "$KH $VK" "$MID $MA" "$MID $MB"; do
  set -- $x
  ip netns exec $1 ethtool -K $2 tso off gso off gro off tx off rx off >/dev/null 2>&1
  ip -n $1 link set dev $2 up
done
AL=""; [ -n "$ALLOW" ] && AL="-allow $ALLOW"
ip netns exec $MID "$W/netem" -a $MA -b $MB -rate "$RATE" -delay "$DELAY" -queue "$QUEUE" \
  -loss "$LOSS" -burstloss "$BURSTLOSS" -badms "$BADMS" -flowrate "$FLOWRATE" $AL ${NETEM_EXTRA:-} \
  2>"$W/netem.log" & echo $! >> "$W/pids"

KEY=$(printf 'ef%.0s' {1..32})
REV=false; [ "$REVERSE" = 1 ] && REV=true
# Direct: iran dials kharej. addr on the dialer is the peer; on the listener the bind.
cat > "$W/kh.json" <<J
{"mode":"listen","carrier":"dgtun","encap":"$ENCAP","proto":$PROTO,"reverse":$REV,
 "addr":"192.168.50.2:2096","iface":"hs0","local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1280,
 "shared_key":"$KEY","expose":"127.0.0.1:5201","forward_ports":"8443",
 "min_links":$MIN,"max_links":$MAX}
J
cat > "$W/ir.json" <<J
{"mode":"dial","carrier":"dgtun","encap":"$ENCAP","proto":$PROTO,"reverse":$REV,
 "addr":"192.168.50.2:2096","iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1280,
 "shared_key":"$KEY","forward_ports":"8443","min_links":$MIN,"max_links":$MAX}
J
# In reverse the dialer is kharej and it needs iran's address; swap addr.
if [ "$REVERSE" = 1 ]; then
  sed -i 's#"addr":"192.168.50.2:2096"#"addr":"192.168.50.1:2096"#' "$W/kh.json"
  sed -i 's#"addr":"192.168.50.2:2096"#"addr":"192.168.50.1:2096"#' "$W/ir.json"
fi

ip netns exec $KH "$W/probe" -server -listen 127.0.0.1:5201 & echo $! >> "$W/pids"
ip netns exec $KH env HS2_NO_TUNE=1 "$BIN" run -c "$W/kh.json" >"$W/kh.log" 2>&1 & echo $! >> "$W/pids"
sleep 1
ip netns exec $IR env HS2_NO_TUNE=1 "$BIN" run -c "$W/ir.json" >"$W/ir.log" 2>&1 & echo $! >> "$W/pids"

# Wait for the tun to come up and carry traffic. The up-check is a TCP connect
# through the forwarded port (which rides the tun), so it is encap-agnostic.
TUNUP=0
for i in $(seq 1 80); do
  if ip -n $IR link show hs0 >/dev/null 2>&1 && ip -n $KH link show hs0 >/dev/null 2>&1; then
    if ip netns exec $IR bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null; then TUNUP=1; break; fi
  fi
  sleep 0.25
done

# Ping is a latency diagnostic. It also covers the icmp encap: its listener
# drops only the kernel's replies to the tunnel's own packets (nft/iptables
# rule keyed on the tunnel magic), so the listener's tun IP still answers ping.
PING=""; PLOSS=""
PINGDST=10.77.0.2
if [ "$TUNUP" = 1 ] && [ -n "$PINGDST" ]; then
  P=$(ip netns exec $IR ping -i 0.2 -w "${T%s}" -q "$PINGDST" 2>/dev/null)
  PING=$(echo "$P" | awk -F'= ' '/rtt/{split($2,a,"/"); print a[2]"/"a[3]}')
  PLOSS=$(echo "$P" | grep -o '[0-9.]*% packet loss' | cut -d% -f1)
fi

# forwarded port: connect to iran 127.0.0.1:8443, throughput to the panel.
PROBE=null
if ip netns exec $IR bash -c 'for i in $(seq 1 40); do exec 3<>/dev/tcp/127.0.0.1/8443 && exit 0; sleep 0.25; done; exit 1' 2>/dev/null; then
  UFLAG=""; [ "${UDP:-0}" = 1 ] && UFLAG="-udp"
  SEGF=""; [ "$SEG" != 0 ] && SEGF="-seg $SEG"
  PROBE=$(ip netns exec $IR "$W/probe" -addr 127.0.0.1:8443 -bulk "$BULK" -up "$UP" -t "$T" $UFLAG $SEGF 2>/dev/null || echo null)
fi
LINKS=$(grep -oE 'carrier .* up \(now [0-9]+\)' "$W/ir.log" | grep -oE 'now [0-9]+' | grep -oE '[0-9]+' | sort -n | tail -1)
[ "${KEEP:-0}" = 1 ] || cleanup
printf '{"encap":"%s","reverse":%s,"rate":"%s","delay":"%s","loss":"%s","tun_up":%s,"ping_avg_max":"%s","ping_loss":"%s","max_links":%s,"probe":%s}\n' \
  "$ENCAP" "$REVERSE" "$RATE" "$DELAY" "$LOSS" "$TUNUP" "${PING:-}" "${PLOSS:-}" "${LINKS:-0}" "${PROBE:-null}"

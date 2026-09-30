#!/usr/bin/env bash
# One hs2 lab experiment: iran <-> [netem] <-> kharej in network namespaces,
# real hs2 binaries on each side, probe traffic through the forwarded port.
# Prints one JSON line. Needs root, iproute2, ethtool, openssl.
#
#   BIN=./hs2 MODE=mtcp RATE=50mbit DELAY=40ms LOSS=0.005 lab/run.sh
#
# Env (defaults in brackets):
#   BIN [./hs2] (BIN_KH: kharej binary if different)  MODE [mtcp]  RATE [50mbit]  DELAY [40ms]  QUEUE [500ms]
#   LOSS [0]  FLOWRATE [0]  BULK [8]  UP [0]  T [20s]  UDP [0]
#   MIN_LINKS [4]  MAX_LINKS [16]
#   IRAN_EXTRA / KHAREJ_EXTRA: extra JSON fields, e.g. '"per_link":4,'
#   KEEP=1 leaves the namespaces and logs in place.
#   PROBE_EXTRA: extra probe flags, e.g. "-seg 4000000" (reconnect per 4 MB download).
#   LAB_ID=n runs in namespaces irn/khn/midn so experiments can run in parallel.
#   HS2_TUNE_* variables are passed through to hs2 (see cmd/hs2 applyTuning).
set -u
D=$(cd "$(dirname "$0")" && pwd)
ID=${LAB_ID:-}
W=${WORKDIR:-/tmp/hs2lab}$ID
IR=ir$ID; KH=kh$ID; MID=mid$ID
# interface names carry the id too: veths are created in the root namespace
# first, so parallel runs must not share names
VI=vi$ID; VK=vk$ID; MA=xa$ID; MB=xb$ID
BIN=${BIN:-./hs2}; BIN_KH=${BIN_KH:-$BIN}; MODE=${MODE:-mtcp}
RATE=${RATE:-50mbit}; DELAY=${DELAY:-40ms}; QUEUE=${QUEUE:-500ms}; LOSS=${LOSS:-0}
FLOWRATE=${FLOWRATE:-0}; BULK=${BULK:-8}; UP=${UP:-0}; T=${T:-20s}; UDP=${UDP:-0}
IRAN_EXTRA=${IRAN_EXTRA:-}; KHAREJ_EXTRA=${KHAREJ_EXTRA:-}

cleanup(){
  for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in $(cat "$W"/pids 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  ip netns del $IR 2>/dev/null; ip netns del $KH 2>/dev/null; ip netns del $MID 2>/dev/null
}
mkdir -p "$W"; cleanup; rm -f "$W"/pids "$W"/*.log
mkdir -p "$W"
[ -x "$W/netem" ] || (cd "$D/.." && go build -o "$W/netem" ./lab/netem) || exit 1
[ -x "$W/probe" ] || (cd "$D/.." && go build -o "$W/probe" ./lab/probe) || exit 1
[ -f "$W/cert.pem" ] || openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/key.pem" \
  -out "$W/cert.pem" -days 30 -subj /CN=lab.example.com 2>/dev/null
cp "$BIN" "$W/hs2"; cp "$BIN_KH" "$W/hs2kh"

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
ip netns exec $MID "$W/netem" -a $MA -b $MB -rate "$RATE" -delay "$DELAY" -queue "$QUEUE" \
  -loss "$LOSS" -flowrate "$FLOWRATE" 2>"$W/netem.log" & echo $! >> "$W/pids"

KEY=$(printf 'ab%.0s' {1..32})
cat > "$W/kh.json" <<J
{"mode":"listen","carrier":"$MODE","addr":"192.168.50.2:2096","iface":"hs0",
 "local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1380,$KHAREJ_EXTRA
 "backend_addr":"builtin","shared_key":"$KEY","cert_file":"$W/cert.pem",
 "key_file":"$W/key.pem","expose":"127.0.0.1:5201"}
J
cat > "$W/ir.json" <<J
{"mode":"dial","carrier":"$MODE","addr":"192.168.50.2:2096","sni":"lab.example.com",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1380,$IRAN_EXTRA
 "shared_key":"$KEY","forward_ports":"8443","min_links":${MIN_LINKS:-4},"max_links":${MAX_LINKS:-16}}
J
ip netns exec $KH "$W/probe" -server -listen 127.0.0.1:5201 & echo $! >> "$W/pids"
ip netns exec $KH env HS2_NO_TUNE=1 "$W/hs2kh" run -c "$W/kh.json" >"$W/kh.log" 2>&1 & echo $! >> "$W/pids"
sleep 1
ip netns exec $IR env HS2_NO_TUNE=1 "$W/hs2" run -c "$W/ir.json" >"$W/ir.log" 2>&1 & echo $! >> "$W/pids"
# wait until the forwarded port answers (max ~15s)
for i in $(seq 1 60); do
  ip netns exec $IR bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break
  sleep 0.25
done
sleep 2
PINGF="$W/ping.txt"; : > "$PINGF"
if ip -n $IR link show hs0 >/dev/null 2>&1; then
  ip netns exec $IR ping -i 0.2 -w "${T%s}" -q 10.77.0.2 > "$PINGF" 2>&1 &
  PP=$!
fi
UFLAG=""; [ "$UDP" = 1 ] && UFLAG="-udp"
OUT=$(ip netns exec $IR "$W/probe" -addr 127.0.0.1:8443 -bulk "$BULK" -up "$UP" -t "$T" $UFLAG ${PROBE_EXTRA:-})
[ -n "${PP:-}" ] && wait "$PP" 2>/dev/null
PING=$(awk -F'= ' '/rtt/{split($2,a,"/"); print a[2]"/"a[3]}' "$PINGF")
PLOSS=$(grep -o '[0-9.]*% packet loss' "$PINGF" | cut -d% -f1)
REAPS=$(grep -cE 'reap:|rebuilt|link down' "$W/ir.log")
[ "${KEEP:-0}" = 1 ] || cleanup
printf '{"mode":"%s","rate":"%s","delay":"%s","loss":"%s","flowrate":"%s","bulk":%s,"ping_avg_max":"%s","ping_loss":"%s","link_churn":%s,"probe":%s}\n' \
  "$MODE" "$RATE" "$DELAY" "$LOSS" "$FLOWRATE" "$BULK" "${PING:-}" "${PLOSS:-}" "$REAPS" "${OUT:-null}"

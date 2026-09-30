#!/usr/bin/env bash
# One datagram-carrier experiment over a chosen encapsulation, across two
# network namespaces joined through the userspace path emulator:
#
#   ir (192.168.50.1) --veth-- mid [netem] --veth-- kh (192.168.50.2)
#
# The real UDP carrier (Noise + ChaCha20-Poly1305 + adaptive FEC + pacer) runs
# on each side via lab/dglab; netem forwards frames of ANY IP protocol, so the
# raw encapsulations (icmp/gre/ipip/ipx) cross it exactly as they would a real
# path. Prints dglab's JSON line with the netem settings merged in.
#
#   ENCAP=icmp RATE=50mbit DELAY=20ms LOSS=0.01 lab/encap.sh
#
# Env [defaults]: ENCAP [udp]  PROTO [0 = ipx default]  RATE [100mbit]
#   DELAY [20ms]  QUEUE [200ms]  LOSS [0]  BURSTLOSS [0] BADMS [0] GOODMS [150]
#   FLOWRATE [0]  ALLOW [""] (e.g. 1 = ICMP-only path)  LINKS [1]  DIR [both]
#   OFFER [0 = as fast as accepted, Mbit/s per direction]  SIZE [1200]  T [8s]
#   LAB_ID [""] for parallel runs; KEEP=1 keeps namespaces and logs.
set -u
D=$(cd "$(dirname "$0")" && pwd)
ID=${LAB_ID:-}
W=${WORKDIR:-/tmp/hs2enc}$ID
IR=eir$ID; KH=ekh$ID; MID=emid$ID
VI=evi$ID; VK=evk$ID; MA=exa$ID; MB=exb$ID
ENCAP=${ENCAP:-udp}; PROTO=${PROTO:-0}
RATE=${RATE:-100mbit}; DELAY=${DELAY:-20ms}; QUEUE=${QUEUE:-200ms}; LOSS=${LOSS:-0}
BURSTLOSS=${BURSTLOSS:-0}; BADMS=${BADMS:-0}; GOODMS=${GOODMS:-150}
FLOWRATE=${FLOWRATE:-0}; ALLOW=${ALLOW:-}; LINKS=${LINKS:-1}; DIR=${DIR:-both}
OFFER=${OFFER:-0}; SIZE=${SIZE:-1200}; T=${T:-8s}

cleanup(){
  for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in $(cat "$W"/pids 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  ip netns del $IR 2>/dev/null; ip netns del $KH 2>/dev/null; ip netns del $MID 2>/dev/null
}
mkdir -p "$W"; cleanup; rm -f "$W"/pids "$W"/*.log
[ -x "$W/netem" ] || (cd "$D/.." && go build -o "$W/netem" ./lab/netem) || exit 1
[ -x "$W/dglab" ] || (cd "$D/.." && go build -o "$W/dglab" ./lab/dglab) || exit 1

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
  -loss "$LOSS" -burstloss "$BURSTLOSS" -badms "$BADMS" -goodms "$GOODMS" -flowrate "$FLOWRATE" $AL \
  2>"$W/netem.log" & echo $! >> "$W/pids"

KEY=$(printf 'cd%.0s' {1..32})
ip netns exec $KH "$W/dglab" -server -encap "$ENCAP" -proto "$PROTO" -listen 192.168.50.2:2096 -key "$KEY" \
  >"$W/srv.log" 2>&1 & echo $! >> "$W/pids"
sleep 0.5
OUT=$(ip netns exec $IR "$W/dglab" -encap "$ENCAP" -proto "$PROTO" -addr 192.168.50.2:2096 -key "$KEY" \
  -links "$LINKS" -dir "$DIR" -rate "$OFFER" -size "$SIZE" -t "$T" 2>"$W/cli.log")
for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
sleep 0.3
NETEM=$(grep -o 'netem stats:.*' "$W/netem.log" | tr -d '\n' | sed 's/"/'"'"'/g')
[ "${KEEP:-0}" = 1 ] || cleanup
printf '{"path":{"rate":"%s","delay":"%s","loss":"%s","burstloss":"%s","badms":"%s","flowrate":"%s","allow":"%s"},"netem":"%s","run":%s}\n' \
  "$RATE" "$DELAY" "$LOSS" "$BURSTLOSS" "$BADMS" "$FLOWRATE" "$ALLOW" "$NETEM" "${OUT:-null}"

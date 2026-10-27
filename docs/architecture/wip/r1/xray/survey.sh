#!/bin/bash
# survey.sh OUT VARIANT...: step 1 of R1 round 3 (xray), outside the shared
# rig. A small lab of three namespaces of its own (xr-c, xr-m, xr-s):
#   xr-c c0 10.231.1.1  <->  xr-m mc | ms (bottleneck: netem RTT/2, htb
#   RATE Mbit/s, fq_codel, each way)  <->  xr-s s0 10.231.2.1
# xr-s runs the REALITY target (tlstarget.py), the traffic server
# (xtraffic.py) and one xray server with every inbound kind (mkxray.py); per
# VARIANT, xr-c runs an xray client with a SOCKS inbound and xtraffic.py's
# "share" timeline through it, then IDLE s more with nothing to send, while
# xr-m records the headers of every packet between the two. RATE (10) keeps
# it light: another agent may be measuring on the shared rig meanwhile.
# Keys and configs live in $KEYS (outside the repo); OUT/raw gets the pcaps
# and logs, OUT/VARIANT.json the summary (survey.py).
set -u
D=$(cd "$(dirname "$0")" && pwd)
OUT=${1:?usage: survey.sh OUT VARIANT...}; shift
mkdir -p "$OUT/raw"; OUT=$(cd "$OUT" && pwd)
KEYS=${KEYS:-/root/rxray/keys}; XR=${XR:-/root/rxray/bin/xray}
RATE=${RATE:-10}; RTT=${RTT:-72}; IDLE=${IDLE:-30}
C=xr-c; M=xr-m; S=xr-s
PIDS=()
declare -A PORT=([xr]=4443 [vv]=4444 [vm]=4445 [tj]=4446 [ws]=4447 [hu]=4448 [gr]=4449 [ss]=4450 [kc]=4451 [xt]=4452)
declare -A KINDOF=([xa]=xr [xc16]=xr [xb1]=xr [xm3]=xr [xb2]=xr [xt]=xt [vv]=vv [vm]=vm [tj]=tj [ws]=ws [hu]=hu [gr]=gr [grm]=gr [ss]=ss [kc]=kc)

down() {
  local n p
  for n in $C $M $S; do for p in $(ip netns pids $n 2>/dev/null); do kill $p 2>/dev/null; done; done
  sleep 0.3
  for n in $C $M $S; do for p in $(ip netns pids $n 2>/dev/null); do kill -9 $p 2>/dev/null; done; ip netns del $n 2>/dev/null; done
  return 0
}
trap 'down; exit 130' INT TERM
trap 'down' EXIT

bneck() {
  local d=$1 half
  half=$(awk -v r="$RTT" 'BEGIN{printf "%.3f", r/2}')
  ip netns exec $M tc qdisc add dev $d root handle 1: netem delay ${half}ms limit 100000
  ip netns exec $M tc qdisc add dev $d parent 1:1 handle 2: htb default 10
  ip netns exec $M tc class add dev $d parent 2: classid 2:10 htb rate ${RATE}mbit ceil ${RATE}mbit burst 16000 cburst 16000
  ip netns exec $M tc qdisc add dev $d parent 2:10 handle 3: fq_codel
}

up() {
  local n x
  down
  for n in $C $M $S; do ip netns add $n && ip -n $n link set lo up || exit 1; done
  ip -n $M link add mc type veth peer name c0 netns $C
  ip -n $M link add ms type veth peer name s0 netns $S
  ip -n $C addr add 10.231.1.1/24 dev c0; ip -n $M addr add 10.231.1.254/24 dev mc
  ip -n $M addr add 10.231.2.254/24 dev ms; ip -n $S addr add 10.231.2.1/24 dev s0
  for x in "$C c0" "$M mc" "$M ms" "$S s0"; do
    set -- $x
    ip netns exec $1 ethtool -K $2 tso off gso off gro off >/dev/null 2>&1
    ip -n $1 link set $2 up
  done
  ip -n $C route add default via 10.231.1.254; ip -n $S route add default via 10.231.2.254
  ip netns exec $M sysctl -qw net.ipv4.ip_forward=1
  ip netns exec $S sysctl -qw net.ipv4.tcp_congestion_control=bbr
  bneck mc; bneck ms
  ip netns exec $C ping -c1 -w5 10.231.2.1 >/dev/null || { echo "lab: no path" >&2; exit 1; }
}

up
# the server side: target, traffic server, one xray server with every kind
[ -f "$KEYS/tls.crt" ] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$KEYS/tls.key" -out "$KEYS/tls.crt" -days 30 -subj /CN=www.example.test \
  -addext subjectAltName=DNS:www.example.test 2>/dev/null
ip netns exec $S python3 "$D/tlstarget.py" "$KEYS/tls.crt" "$KEYS/tls.key" 127.0.0.1:4431 > "$OUT/raw/tlstarget.log" 2>&1 &
ip netns exec $S python3 "$D/xtraffic.py" server > "$OUT/raw/xtraffic-server.log" 2>&1 &
ib=$(for k in "${!PORT[@]}"; do printf '%s:%s,' "$k" "${PORT[$k]}"; done)
python3 "$D/mkxray.py" server "$KEYS/keys.env" "$KEYS/server.json" --listen 10.231.2.1 --target 127.0.0.1:4431 \
  --cert "$KEYS/tls.crt" --key "$KEYS/tls.key" --access "$OUT/raw/server-access.log" \
  --error "$OUT/raw/server-error.log" --loglevel info --inbounds "${ib%,}" || exit 1
ip netns exec $S "$XR" run -c "$KEYS/server.json" > "$OUT/raw/server-stdout.log" 2>&1 &
sleep 1.5
for v in "$@"; do
  k=${KINDOF[$v]:?unknown variant $v}; p=${PORT[$k]}
  python3 "$D/mkxray.py" client "$KEYS/keys.env" "$KEYS/client-$v.json" --variant $v --addr 10.231.2.1 --port $p \
    --loglevel debug --error "$OUT/raw/$v-client.log" || exit 1
  a0=$(wc -l < "$OUT/raw/server-access.log" 2>/dev/null || echo 0)
  ip netns exec $M tcpdump -i mc -s 64 -U -w "$OUT/raw/$v.pcap" 'host 10.231.2.1' > /dev/null 2>&1 &
  td=$!
  sleep 0.5
  ip netns exec $C "$XR" run -c "$KEYS/client-$v.json" > "$OUT/raw/$v-client-stdout.log" 2>&1 &
  xc=$!
  sleep 1
  echo "$(date +%T) $v: share timeline"
  ip netns exec $C python3 "$D/xtraffic.py" client --scenario share --out "$OUT/raw/$v-flows.json" 2> "$OUT/raw/$v-flows.err"
  # idle: nothing to send; who keeps or closes the connections
  for i in $(seq 1 $((IDLE / 5))); do
    echo "T $(date +%s.%N)" >> "$OUT/raw/$v-ss.txt"
    ip netns exec $C ss -tuniH dst 10.231.2.1 >> "$OUT/raw/$v-ss.txt" 2>/dev/null
    sleep 5
  done
  kill $xc; sleep 1; kill $td; wait $td 2>/dev/null
  sleep 0.5
  tail -n +$((a0 + 1)) "$OUT/raw/server-access.log" > "$OUT/raw/$v-access.log"
  python3 "$D/survey.py" "$OUT/raw" $v --json "$OUT/$v.json" | tee -a "$OUT/survey.txt"
  sleep 2
done

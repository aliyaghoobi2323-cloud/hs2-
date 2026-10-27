#!/bin/bash
# game.sh OUT LABEL N LOSS REP [MIX]: one run of the UDP game measurement
# (r1-udp, R1 round 3). The rig (../../rig-scale/rig.sh, RIG_ID=u) at 50
# Mbit/s, RTT 72 ms, fq_codel, LOSS % each way; the reverse l3mtcp tunnel
# with "udp": true and N links (min = max = N); BIN and BENV (the binary of
# LABEL and its HS2_* switches) passed by the caller in the environment.
#   MIX bulk (default): user 1 downloads over 16 TCP connections through user
#                       port 5311, user 2 probes (TCP, 32 B each 200 ms)
#       idle          : the probe only (the game flow's floor without bulk)
# User 3 plays a game over UDP port 5311: 64 Hz x 60-120 B up, 30 Hz x
# 100-600 B down (udpgame). The exit's panel answers TCP (rigload) and UDP
# (udpgame) on 127.0.0.1:5211. WARM 5 s, DUR 40 s. Raw files to
# /root/rudp/raw-game/<id>/; one JSON line to OUT/res.jsonl (gana.py).
set -u
D=$(cd "$(dirname "$0")" && pwd)
RS=$D/../../rig-scale
OUT=${1:?OUT}; LABEL=${2:?LABEL}; N=${3:?N}; LOSS=${4:?LOSS}; REP=${5:?REP}; MIX=${6:-bulk}
BIN=${BIN:?BIN}; BENV=${BENV:-}
export RIG_ID=${RIG_ID:-u}
E=r-edge$RIG_ID; X=r-exit$RIG_ID
WARM=5; DUR=40; TOTAL=$((WARM + DUR))
mkdir -p "$OUT"; OUT=$(cd "$OUT" && pwd)
ID="$MIX-n$N-l$LOSS-$LABEL-$REP"
R=/root/rudp/raw-game/$ID; rm -rf "$R"; mkdir -p "$R"
W=$(mktemp -d /tmp/rudp-g.XXXXXX)
RL=/root/rudp/tools/rigload; UG=/root/rudp/tools/udpgame
PIDS=()
cleanup() {
  local p s
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in "${PIDS[@]}"; do kill -9 "$p" 2>/dev/null; done
  "$RS/rig.sh" down
  s=/run/hs2/$(echo "${W#/}" | tr / -)
  rm -f "$s"-*.json.status.json "$s"-*.json.warm
  rm -rf "$W"
}
trap 'cleanup; exit 130' INT TERM
trap cleanup EXIT

RATE=50 RTT=72 LOSS=$LOSS MODE=fqcodel NUSERS=3 V6=0 NTUN=0 "$RS/rig.sh" up 2> "$R/rig.err" || { echo "rig up: $(tail -1 "$R/rig.err")"; exit 1; }
key=$(openssl rand -hex 32)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$W/key.pem" -out "$W/cert.pem" -days 2 -subj /CN=rig.example.com 2>/dev/null
cat > "$W/edge.json" <<J
{"mode":"dial","reverse":true,"carrier":"l3mtcp","udp":true,"addr":"10.201.0.1:2096",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1320,
 "shared_key":"$key","cert_file":"$W/cert.pem","key_file":"$W/key.pem","backend_addr":"builtin",
 "forward_ports":"5311,5312","min_links":$N,"max_links":$N,"per_link":8}
J
cat > "$W/exit.json" <<J
{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"10.201.0.1:2096","sni":"rig.example.com",
 "bind_local_ip":"10.202.0.1","iface":"hs0","local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1320,
 "shared_key":"$key","expose":"127.0.0.1:5211","port_map":"5311=127.0.0.1:5211,5312=127.0.0.1:5212",
 "min_links":$N,"max_links":$N,"per_link":8}
J
chmod 600 "$W"/*.json "$W/key.pem"
CELL="N=$N RATE=50 RTT=72 LOSS=$LOSS MODE=fqcodel USERS=2 MIX=lone16 WARM=$WARM DUR=$DUR"
[ "$MIX" = idle ] && CELL="N=$N RATE=50 RTT=72 LOSS=$LOSS MODE=fqcodel USERS=1 MIX=s5 DN=0 UP=0 WARM=$WARM DUR=$DUR"
read -r nusers total ntun < <(python3 "$RS/ana.py" spec "$CELL" "$R/spec.json" 2> "$R/spec.err")
[ -n "${total:-}" ] || { echo "spec: $(tail -1 "$R/spec.err")"; exit 1; }

ip netns exec $X "$RL" server -listen 127.0.0.1:5211,127.0.0.1:5212 > "$R/rigload-server.log" 2>&1 &
PIDS+=($!)
ip netns exec $X "$UG" server -listen 127.0.0.1:5211 -dur $((TOTAL + 20)) -out "$R/game-server.json" > "$R/game-server.log" 2>&1 &
PIDS+=($!); PG=$!
ip netns exec $E env HS2_NO_TUNE=1 $BENV "$BIN" run -c "$W/edge.json" > "$R/edge.log" 2>&1 &
PE=$!; PIDS+=($PE)
sleep 0.5
ip netns exec $X env HS2_NO_TUNE=1 $BENV "$BIN" run -c "$W/exit.json" > "$R/exit.log" 2>&1 &
PX=$!; PIDS+=($PX)
n=$("$RS/rig.sh" wait-links "$N" 40)
[ "${n:-0}" -ge "$N" ] || { echo "only ${n:-0} of $N links"; exit 1; }
sleep 2
ip netns exec $E timeout $((TOTAL + 30)) "$RL" client -spec "$R/spec.json" > "$R/client.json" 2> "$R/client.err" &
PC=$!; PIDS+=($PC)
ip netns exec $E "$UG" client -to 10.201.0.1:5311 -src 10.100.0.3 -flow 1 -dur $TOTAL -out "$R/game-client.json" > "$R/game-client.log" 2>&1 &
PU=$!; PIDS+=($PU)
T0=$(date +%s%N)
sleep $((WARM + 2))
ip netns exec $X ss -tinH state established '( dport = :2096 )' > "$R/ss-exit.txt" 2>/dev/null
wait $PU; wait $PC
ip netns exec $E ss -tinH state established '( sport = :2096 )' > "$R/ss-edge-end.txt" 2>/dev/null
"$BIN" status -c "$W/edge.json" > "$R/status-edge.txt" 2>&1
sleep 1
# the game server writes its file on SIGTERM (the client has ended)
kill -TERM $PG 2>/dev/null
for i in $(seq 1 30); do [ -s "$R/game-server.json" ] && break; sleep 0.5; done
ALIVE=1; kill -0 $PE 2>/dev/null || ALIVE=0
python3 "$D/gana.py" run "$R" "$ID" "$LABEL" "$N" "$LOSS" "$REP" "$MIX" "$T0" "$WARM" "$DUR" "$ALIVE" >> "$OUT/res.jsonl"
tail -1 "$OUT/res.jsonl"

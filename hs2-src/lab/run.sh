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
#   KH_QDISC / IR_QDISC: a qdisc on the Kharej / Iran side's link into the path, e.g. "cake bandwidth 92mbit".
#   NETEM_EXTRA: extra emulator flags, e.g. "-burstloss 0.3 -badms 30 -goodms 2000" (bursty loss).
#   PROBE_EXTRA: extra probe flags, e.g. "-seg 4000000" (reconnect per 4 MB download).
#   CALL [] e.g. 2mbit: a two-way video-call-like stream (probe -call)   GAME [0] 1 = game-like stream
#   LATE [0] e.g. 5s: echo/call/game and the hs0 ping start this long after the downloads
#   CROWD [0] e.g. 300: light connections (probe -crowd) so the pool grows to tens of links;
#     CROWD_EVERY [700ms] mean interval of their requests. Raise MAX_LINKS with it (e.g. 128).
#   The hs0 ping (modes with a TUN, e.g. l3mtcp) is `ping -i 0.2` to the other end's inner
#   address; its per-reply times give ping_p50/p90/p99 and ping_over120_pct (ms, replies only).
#   WEB [0] 1 = page loads (probe -web): every 2-3 s, 4 parallel short connections of 50-500 KB.
#   VIA [] tun = all probe traffic (downloads, echo, call, game, web, crowd) goes to the kharej
#     probe server at the other end's inner TUN address 10.77.0.2:5201 through hs0 instead of the
#     forwarded port 127.0.0.1:8443 (needs a TUN mode, e.g. l3mtcp). The JSON then also has "via",
#     "tun_route" (`ip route get` in the Iran namespace) and the Iran hs0 byte counters over the
#     probe run (tun_rx_mb / tun_tx_mb).
#   JOURNEY [0] e.g. 20: virtual users (probe -journey) each doing reels -> video call -> big
#     download -> app pings -> game in turn, starts spread over 10 s; JSON "journey" aggregates
#     every phase over all users. JOURNEY_EXTRA: its flags, e.g. "-journeyscale 0.5 -jreels 3".
#     Combines with BULK (set BULK=0 for the journey alone), CROWD, CALL, GAME, WEB.
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
VIA=${VIA:-}; WEB=${WEB:-0}
# PATHS=2: a second Iran<->kharej IP pair (192.168.51.0/24) with its own netem
# (PATH_A_NETEM / PATH_B_NETEM: extra netem flags per path), and the tunnel in
# reverse with multipath lb on; LB_WEIGHTS="A,B" are the Iran IPs' weights.
PATHS=${PATHS:-1}; PATH_A_NETEM=${PATH_A_NETEM:-}; PATH_B_NETEM=${PATH_B_NETEM:-}; LB_WEIGHTS=${LB_WEIGHTS:-50,50}
# LB_MODE=fixed|auto is lb.mode; auto runs with HS2_LB_FAST=1 (quality tick
# 5 s, rebalance 10 s instead of 30 s / 60 s) so a 60 s run sees it adapt.
LB_MODE=${LB_MODE:-fixed}
if [ "$LB_MODE" = auto ]; then export HS2_LB_FAST=${HS2_LB_FAST:-1}; fi
JOURNEY=${JOURNEY:-0}; JOURNEY_EXTRA=${JOURNEY_EXTRA:-}
CALL=${CALL:-}; GAME=${GAME:-0}; LATE=${LATE:-0}; CROWD=${CROWD:-0}; CROWD_EVERY=${CROWD_EVERY:-700ms}

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
[ -n "${KH_QDISC:-}" ] && ip netns exec $KH tc qdisc replace dev $VK root ${KH_QDISC}
[ -n "${IR_QDISC:-}" ] && ip netns exec $IR tc qdisc replace dev $VI root ${IR_QDISC}
ip netns exec $MID "$W/netem" -a $MA -b $MB -rate "$RATE" -delay "$DELAY" -queue "$QUEUE" \
  -loss "$LOSS" -flowrate "$FLOWRATE" ${NETEM_EXTRA:-} $PATH_A_NETEM 2>"$W/netem.log" & echo $! >> "$W/pids"
if [ "$PATHS" = 2 ]; then
  ip link add ${VI}b netns $IR type veth peer name ${MA}b netns $MID
  ip link add ${VK}b netns $KH type veth peer name ${MB}b netns $MID
  ip -n $IR addr add 192.168.51.1/24 dev ${VI}b
  ip -n $KH addr add 192.168.51.2/24 dev ${VK}b
  for x in "$IR ${VI}b" "$KH ${VK}b" "$MID ${MA}b" "$MID ${MB}b"; do
    set -- $x
    ip netns exec $1 ethtool -K $2 tso off gso off gro off tx off rx off >/dev/null 2>&1
    ip -n $1 link set dev $2 up
  done
  # replies from the second Iran IP go back over path B
  ip -n $IR rule add from 192.168.51.1 table 151
  ip -n $IR route add 192.168.50.2/32 dev ${VI}b src 192.168.51.1 table 151
  ip netns exec $MID "$W/netem" -a ${MA}b -b ${MB}b -rate "$RATE" -delay "$DELAY" -queue "$QUEUE" \
    -loss "$LOSS" -flowrate "$FLOWRATE" ${NETEM_EXTRA:-} $PATH_B_NETEM 2>"$W/netem-b.log" & echo $! >> "$W/pids"
  WA=${LB_WEIGHTS%,*}; WB=${LB_WEIGHTS#*,}
  LBJ='"lb":{"enabled":true,"mode":"'$LB_MODE'","iran":[{"ip":"192.168.50.1","weight":'$WA'},{"ip":"192.168.51.1","weight":'$WB'}],"kharej":[{"ip":"192.168.50.2","weight":100}]},'
  KHAREJ_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","bind_local_ip":"192.168.50.2","sni":"lab.example.com",'"$LBJ$KHAREJ_EXTRA"
  IRAN_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","backend_addr":"builtin","cert_file":"'$W'/cert.pem","key_file":"'$W'/key.pem",'"$LBJ$IRAN_EXTRA"
fi

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
# VIA=tun: the server must also answer on the inner address, which only exists
# once hs2 has made hs0, so it listens on all addresses
PLISTEN=127.0.0.1:5201; [ "$VIA" = tun ] && PLISTEN=0.0.0.0:5201
ip netns exec $KH "$W/probe" -server -listen $PLISTEN & echo $! >> "$W/pids"
ip netns exec $KH env HS2_NO_TUNE=1 "$W/hs2kh" run -c "$W/kh.json" >"$W/kh.log" 2>&1 & echo $! >> "$W/pids"
sleep 1
ip netns exec $IR env HS2_NO_TUNE=1 "$W/hs2" run -c "$W/ir.json" >"$W/ir.log" 2>&1 & echo $! >> "$W/pids"
# wait until the forwarded port answers (max ~15s)
for i in $(seq 1 60); do
  ip netns exec $IR bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break
  sleep 0.25
done
PADDR=127.0.0.1:8443; TUNX=""
if [ "$VIA" = tun ]; then
  PADDR=10.77.0.2:5201
  for i in $(seq 1 60); do
    ip netns exec $IR bash -c 'exec 3<>/dev/tcp/10.77.0.2/5201' 2>/dev/null && break
    sleep 0.25
  done
  ROUTE=$(ip -n $IR route get 10.77.0.2 2>&1 | head -1 | tr -s ' ' | tr -d '"' | sed 's/ *$//')
  case "$ROUTE" in
    *"dev hs0 "*|*"dev hs0") ;;
    *) echo "VIA=tun: 10.77.0.2 is not routed through hs0: $ROUTE" >&2; cleanup; exit 1 ;;
  esac
  tunb(){ ip netns exec $IR cat /sys/class/net/hs0/statistics/$1 2>/dev/null || echo 0; }
fi
sleep 2
PINGF="$W/ping.txt"; : > "$PINGF"
if ip -n $IR link show hs0 >/dev/null 2>&1; then
  # with LATE the ping starts with the late streams and ends with the probe
  PW=$(python3 -c "import sys; t=float(sys.argv[1].rstrip('s')); l=float(sys.argv[2].rstrip('s') or 0); print(max(int(t-l),1))" "$T" "$LATE")
  (sleep "${LATE%s}"; exec ip netns exec $IR ping -n -i 0.2 -w "$PW" 10.77.0.2) > "$PINGF" 2>&1 &
  PP=$!
fi
SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
# the pool's link count once a second while the probe runs (links_max)
(while :; do python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('links',0))" "$SF" 2>/dev/null; sleep 1; done) > "$W/links.txt" &
LP=$!; echo $LP >> "$W/pids"
SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
if [ "$PATHS" = 2 ]; then
  (while :; do python3 -c "import json,sys; print(' '.join(str(p['links']) for p in json.load(open(sys.argv[1])).get('lb',[])))" "$SFK" 2>/dev/null; sleep 1; done) > "$W/lbpaths.txt" &
  echo $! >> "$W/pids"
fi
UFLAG=""; [ "$UDP" = 1 ] && UFLAG="-udp"
PX=""
[ -n "$CALL" ] && PX="$PX -call $CALL"
[ "$GAME" = 1 ] && PX="$PX -game"
[ "$WEB" = 1 ] && PX="$PX -web"
[ "$LATE" != 0 ] && PX="$PX -late $LATE"
[ "$CROWD" != 0 ] && PX="$PX -crowd $CROWD -crowdevery $CROWD_EVERY"
[ "$JOURNEY" != 0 ] && PX="$PX -journey $JOURNEY $JOURNEY_EXTRA"
[ "$VIA" = tun ] && { RX0=$(tunb rx_bytes); TX0=$(tunb tx_bytes); }
OUT=$(ip netns exec $IR "$W/probe" -addr $PADDR -bulk "$BULK" -up "$UP" -t "$T" $UFLAG $PX ${PROBE_EXTRA:-})
if [ "$VIA" = tun ]; then
  # Iran side hs0: rx = what came back through the TUN (downloads, replies), tx = what went into it
  TUNX=$(printf '"via":"tun","tun_route":"%s","tun_rx_mb":%s,"tun_tx_mb":%s,' "$ROUTE" \
    "$(python3 -c "import sys; print(round((int(sys.argv[2])-int(sys.argv[1]))/1e6,1))" "$RX0" "$(tunb rx_bytes)")" \
    "$(python3 -c "import sys; print(round((int(sys.argv[2])-int(sys.argv[1]))/1e6,1))" "$TX0" "$(tunb tx_bytes)")")
fi
[ -n "${PP:-}" ] && wait "$PP" 2>/dev/null
kill $LP 2>/dev/null
LMAX=$(sort -n "$W/links.txt" 2>/dev/null | tail -1)
PING=$(awk -F'= ' '/rtt/{split($2,a,"/"); print a[2]"/"a[3]}' "$PINGF")
PLOSS=$(grep -o '[0-9.]*% packet loss' "$PINGF" | cut -d% -f1)
# per-reply hs0 ping times -> "ping_n":..,"ping_p50":..,... (empty without a TUN)
PSTAT=$(python3 - "$PINGF" <<'PY'
import re, sys
v = sorted(float(x) for x in re.findall(r'time=([0-9.]+) ms', open(sys.argv[1]).read()))
if v:
    p = lambda q: v[int(q * (len(v) - 1))]
    over = 100 * sum(x > 120 for x in v) / len(v)
    print('"ping_n":%d,"ping_p50":%.1f,"ping_p90":%.1f,"ping_p99":%.1f,"ping_max":%.1f,"ping_over120_pct":%.2f,'
          % (len(v), p(.5), p(.9), p(.99), v[-1], over))
PY
)
REAPS=$(grep -cE 'reap:|rebuilt|link down' "$W/ir.log")
LINKS=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get(\"links\",0))" "$SF" 2>/dev/null || echo 0)
[ "${LINKS:-0}" -gt "${LMAX:-0}" ] 2>/dev/null && LMAX=$LINKS
QCAPS=$(grep -c "tunnel egress capped" "$W/kh.log" 2>/dev/null)
QREL=$(grep -c "not the tunnel's own" "$W/kh.log" 2>/dev/null)
LBX=""
if [ "$PATHS" = 2 ]; then
  # per-path link counts on the dialing (kharej) side: at the end, and the max
  LBX=$(python3 - "$W/lbpaths.txt" "$SFK" <<'PY'
import json, sys
rows = [list(map(int, l.split())) for l in open(sys.argv[1]) if l.split()]
try:
    st = json.load(open(sys.argv[2]))
    end = [p['links'] for p in st.get('lb', [])]; text = st.get('lb_text', '')
except Exception:
    end, text = (rows[-1] if rows else []), ''
mx = [max(r[i] for r in rows if len(r) > i) for i in range(len(end))] if rows else []
half = rows[len(rows)//2:]
avg = [round(sum(r[i] for r in half if len(r) > i) / max(1, len(half)), 1) for i in range(len(end))]
print('"lb_paths_end":%s,"lb_paths_max":%s,"lb_paths_avg2h":%s,"lb_text":"%s",' % (json.dumps(end), json.dumps(mx), json.dumps(avg), text))
PY
)
fi
[ "${KEEP:-0}" = 1 ] || cleanup
printf '{"qcaps":%s,"qrel":%s,"links_end":%s,"links_max":%s,"mode":"%s","rate":"%s","delay":"%s","loss":"%s","flowrate":"%s","bulk":%s,"ping_avg_max":"%s","ping_loss":"%s",%s%s"link_churn":%s,%s"probe":%s}\n' \
  "${QCAPS:-0}" "${QREL:-0}" "${LINKS:-0}" "${LMAX:-0}" "$MODE" "$RATE" "$DELAY" "$LOSS" "$FLOWRATE" "$BULK" "${PING:-}" "${PLOSS:-}" "${PSTAT:-}" "$TUNX" "$REAPS" "$LBX" "${OUT:-null}"

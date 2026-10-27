#!/bin/bash
# repro.sh OUT TOPO UDP COND [COND2 ...]: one case of the r1-udp reproduction
# matrix (R1 round 3, the "udp": true report). It brings up the rig's three
# namespaces (../../rig-scale/rig.sh, RIG_ID=u by default), writes an edge and
# an exit config exactly as the installer's heredocs write them for TOPO, runs
# the two hs2 (BIN, default the production build copy), and checks: is the
# edge alive, how many links, does a TCP user port round trip work, does a UDP
# user port round trip work (datagrams to an echo at the exit's panel address).
# One JSON line per case goes to OUT/res.jsonl; logs to OUT/raw-<case>/.
#
#   TOPO  rev-l3mtcp (production) | rev-mtcp | rev-tls | dir-mtcp | dir-l3mtcp | dir-tls
#   UDP   true | false   (the edge's top-level "udp", as the installer's y/N writes it)
#   COND  base         nothing special
#         nopanel      the panel address has TCP only (no UDP inbound), as on the
#                      production exit (xray TCP inbound)
#         collide      another program holds UDP [::]:8443 on the edge before hs2 starts
#         collide4     ... holds UDP 0.0.0.0:8443 (IPv4 only)
#         collideip    ... holds UDP 10.201.0.1:8443 (one address)
#         fwdrop       the edge drops UDP to 8443 (iptables INPUT, in its namespace)
#         userip       user_listen_ip 10.201.0.1 instead of all
#         userip6      user_listen_ip :: (all, IPv6 written out)
#         v6           the user comes over IPv6 (fd77:1::/64 -> [fd00:201::1]:8443)
#         spray        3000 one-datagram flows from fresh source ports in ~3 s
#                      first (scanner/QUIC-probe like), then the checks
#         late         the colliding UDP program starts 5 s AFTER hs2 (it loses)
#         release      (with collide*) the other program lets the port go once the
#                      links are up; the UDP check runs 4 s later
#         secondip     the edge has a second address 10.201.0.2 on e0 and the user
#                      (in r-mid, 10.201.0.254) sends to it with a CONNECTED socket,
#                      as game clients do: are the replies from the address it sent to?
#         noipv6       IPv6 switched off in the edge's namespace (disable_ipv6=1 on
#                      all, default, lo and e0), as on many Iran VPSes
#
# Never touches the root namespace, /etc/hs2 or the production service.
set -u
D=$(cd "$(dirname "$0")" && pwd)
RIG=${RIG:-$D/../../rig-scale/rig.sh}
OUT=${1:?OUT}; TOPO=${2:?TOPO}; UDP=${3:?UDP}; shift 3
CONDS="$*"; [ -n "$CONDS" ] || CONDS=base
BIN=${BIN:-/root/rudp/bins/hs2-prod}
export RIG_ID=${RIG_ID:-u}
E=r-edge$RIG_ID; M=r-mid$RIG_ID; X=r-exit$RIG_ID
mkdir -p "$OUT"; OUT=$(cd "$OUT" && pwd)
CASE="$TOPO-udp$UDP-$(echo $CONDS | tr ' ' '+')-$(basename "$BIN")"
R=$OUT/raw-$CASE; rm -rf "$R"; mkdir -p "$R"
W=$(mktemp -d /tmp/rudp-w.XXXXXX)
PIDS=()
has() { case " $CONDS " in *" $1 "*) return 0 ;; esac; return 1; }
nsx() { local n=$1; shift; ip netns exec "$n" "$@"; }

cleanup() {
  local p s
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in "${PIDS[@]}"; do kill -9 "$p" 2>/dev/null; done
  "$RIG" down
  s=/run/hs2/$(echo "${W#/}" | tr / -)
  rm -f "$s"-*.json.status.json "$s"-*.json.warm
  rm -rf "$W"
}
trap 'cleanup; exit 130' INT TERM
trap cleanup EXIT

case "$TOPO" in
  rev-l3mtcp) DIR=reverse CAR=l3mtcp TR=tun ;;
  rev-mtcp)   DIR=reverse CAR=mtcp   TR=tcp ;;
  rev-tls)    DIR=reverse CAR=tls    TR=tun ;;
  dir-mtcp)   DIR=direct  CAR=mtcp   TR=tcp ;;
  dir-l3mtcp) DIR=direct  CAR=l3mtcp TR=tun ;;
  dir-tls)    DIR=direct  CAR=tls    TR=tun ;;
  *) echo "unknown TOPO $TOPO" >&2; exit 2 ;;
esac
case "$UDP" in true|false) ;; *) echo "UDP true|false" >&2; exit 2 ;; esac

# the rig: 100 Mbit/s, 20 ms, fq_codel; two IPv4 users and the IPv6 /64
RATE=100 RTT=20 LOSS=0 MODE=fqcodel NUSERS=2 V6=1 NTUN=0 "$RIG" up 2> "$R/rig.err" || { echo "rig up failed: $(tail -1 "$R/rig.err")" >&2; exit 1; }

# keys and cert per case (never in the repo)
SHARED=$(openssl rand -hex 32); SEED=$(openssl rand -hex 16)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$W/key.pem" -out "$W/cert.pem" -days 2 -subj /CN=rig.example.com 2>/dev/null
DOMAIN=rig.example.com; TPORT=2096; PORTS=8443; PANEL=127.0.0.1:8443; MTU=1320
EIP=10.201.0.1; XIP=10.202.0.1
USERIP=""; has userip && USERIP=$EIP; has userip6 && USERIP="::"
# the installer's link_json
link_json() { local u=false; [ "$4" = true ] && u=true
  printf '"link": {"endpoint": "%s", "domain": "%s", "transport": "%s", "udp": %s, "panel": "%s"}' "$1" "$2" "$3" "$u" "$5"; }
[ "$TR" = tcp ] && TMTU=1380 || TMTU=$MTU

if [ "$DIR" = reverse ]; then
  # iran_listener (tcp branch, or the tun-over-TLS branch): the edge LISTENS
  cat > "$W/edge.json" <<EOF
{
  "mode": "dial", "carrier": "$CAR", "reverse": true, "udp": $UDP,
  "addr": "$EIP:$TPORT",
  "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": $TMTU,
  "backend_addr": "builtin",
  "cover_seed": "$SEED",
  "shared_key": "$SHARED",
  "cert_file": "$W/cert.pem", "key_file": "$W/key.pem",
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP",
  "min_links": 2, "max_links": 0, "per_link": 8,
  $(link_json "$EIP:$TPORT" "$DOMAIN" "$TR" "$UDP" "-")
}
EOF
  # kharej_dialer (same branch): the exit DIALS the edge
  cat > "$W/exit.json" <<EOF
{
  "mode": "listen", "carrier": "$CAR", "reverse": true,
  "addr": "$EIP:$TPORT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": $TMTU,
  "shared_key": "$SHARED",
  "expose": "$PANEL",
  "min_links": 2, "max_links": 0, "per_link": 8,
  "bind_local_ip": "$XIP",
  $(link_json "$EIP:$TPORT" "$DOMAIN" "$TR" "$UDP" "-")
}
EOF
else
  # kharej_listener (tcp or tun-over-TLS branch): the exit LISTENS; its
  # link block carries the udp answer, the edge writes it at the top level
  cat > "$W/exit.json" <<EOF
{
  "mode": "listen", "carrier": "$CAR", "reverse": false,
  "addr": "$XIP:$TPORT",
  "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": $TMTU,
  "backend_addr": "builtin",
  "cover_seed": "$SEED",
  "shared_key": "$SHARED",
  "cert_file": "$W/cert.pem", "key_file": "$W/key.pem",
  "expose": "$PANEL",
  $(link_json "$XIP:$TPORT" "$DOMAIN" "$TR" "$UDP" "$PANEL")
}
EOF
  # iran_dialer (same branch)
  cat > "$W/edge.json" <<EOF
{
  "mode": "dial", "carrier": "$CAR", "reverse": false, "udp": $UDP,
  "addr": "$XIP:$TPORT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": $TMTU,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL", "user_listen_ip": "$USERIP",
  "min_links": 2, "max_links": 0, "per_link": 8,
  "bind_local_ip": "",
  $(link_json "$XIP:$TPORT" "$DOMAIN" "$TR" "$UDP" "$PANEL")
}
EOF
fi
chmod 600 "$W"/*.json "$W/key.pem"
# what the installer's write_cfg_checked runs (hs2 check), in each namespace
nsx $E "$BIN" check -c "$W/edge.json" > "$R/check-edge.txt" 2>&1; CE=$?
nsx $X "$BIN" check -c "$W/exit.json" > "$R/check-exit.txt" 2>&1; CX=$?
sed -e "s/$SHARED/<key>/g" -e "s/$SEED/<seed>/g" "$W/edge.json" > "$R/edge.json"
sed -e "s/$SHARED/<key>/g" -e "s/$SEED/<seed>/g" "$W/exit.json" > "$R/exit.json"

# the panel on the exit: TCP echo always, UDP echo unless nopanel
PU=""; has nopanel && PU=--no-udp
nsx $X python3 "$D/udpt.py" echo $PANEL $PU > "$R/panel.log" 2>&1 &
PIDS+=($!)
# a program holding the UDP port on the edge
case " $CONDS " in
  *" collide "*)   nsx $E python3 "$D/udpt.py" hold "[::]:$PORTS" > "$R/hold.log" 2>&1 & HOLD=$!; PIDS+=($!) ;;
  *" collide4 "*)  nsx $E python3 "$D/udpt.py" hold "0.0.0.0:$PORTS" > "$R/hold.log" 2>&1 & HOLD=$!; PIDS+=($!) ;;
  *" collideip "*) nsx $E python3 "$D/udpt.py" hold "$EIP:$PORTS" > "$R/hold.log" 2>&1 & HOLD=$!; PIDS+=($!) ;;
esac
has secondip && ip -n $E addr add 10.201.0.2/24 dev e0
has fwdrop && nsx $E iptables -I INPUT -p udp --dport $PORTS -j DROP
has noipv6 && nsx $E sysctl -qw net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1   net.ipv6.conf.lo.disable_ipv6=1 net.ipv6.conf.e0.disable_ipv6=1
sleep 0.5

start() { # side
  nsx "$1" env HS2_NO_TUNE=1 "$BIN" run -c "$W/$2.json" > "$R/$2.log" 2>&1 &
  PIDS+=($!); eval "P_$2=$!"
}
T0=$(date +%s.%N)
if [ "$DIR" = reverse ]; then start $E edge; sleep 0.5; start $X exit
else start $X exit; sleep 0.5; start $E edge; fi
if has late; then
  sleep 5
  nsx $E python3 "$D/udpt.py" hold "[::]:$PORTS" > "$R/hold.log" 2>&1 & PIDS+=($!)
fi

# links: established TCP connections on the tunnel port, seen from the edge
links() { nsx $E ss -Htn state established "( sport = :$TPORT or dport = :$TPORT )" 2>/dev/null | wc -l; }
L=0; UPS=""
for i in $(seq 1 100); do
  L=$(links)
  if [ "$L" -ge 1 ]; then UPS=$(awk -v a="$T0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b - a}'); break; fi
  kill -0 $P_edge 2>/dev/null || break
  sleep 0.25
done
sleep 3   # the links' info exchange (wire v2: users wait for it)

if has spray; then
  nsx $E python3 - "$EIP" "$PORTS" > "$R/spray.log" 2>&1 <<'PY'
import socket, sys, time
host, port = sys.argv[1], int(sys.argv[2])
t = time.time(); socks = []
for i in range(3000):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind(("10.100.0.2", 0)); s.sendto(b"probe-%d" % i, (host, port)); socks.append(s)
    if len(socks) >= 500:
        for x in socks: x.close()
        socks = []
    time.sleep(0.001)
print("sprayed 3000 flows in %.1f s" % (time.time() - t))
PY
  sleep 2
fi

if has release && [ -n "${HOLD:-}" ]; then pkill -P $HOLD 2>/dev/null; kill $HOLD 2>/dev/null; sleep 4; fi
if has v6; then UA="[fd00:201::1]:$PORTS"; SRC=fd77:1::5; else UA="$EIP:$PORTS"; SRC=10.100.0.1; fi
EALIVE=0; kill -0 $P_edge 2>/dev/null && EALIVE=1
XALIVE=0; kill -0 $P_exit 2>/dev/null && XALIVE=1
if has secondip; then
  TC=$(nsx $M python3 "$D/udpt.py" tcpcheck "10.201.0.2:$PORTS" --n 5)
  UC=$(nsx $M python3 "$D/udpt.py" udpcheck "10.201.0.2:$PORTS" --n 20 --connected)
else
TC=$(nsx $E python3 "$D/udpt.py" tcpcheck "$UA" --src $SRC --n 5)
UC=$(nsx $E python3 "$D/udpt.py" udpcheck "$UA" --src $SRC --n 20)
fi
L2=$(links)
EALIVE2=0; kill -0 $P_edge 2>/dev/null && EALIVE2=1
sleep 0.5
nsx $E "$BIN" status -c "$W/edge.json" > "$R/status-edge.txt" 2>&1
# the edge's own sockets on the user port
nsx $E ss -Hlnup "( sport = :$PORTS )" > "$R/ss-edge-udp.txt" 2>&1
nsx $E ss -Hlntp "( sport = :$PORTS )" > "$R/ss-edge-tcp.txt" 2>&1
FATAL=$(grep -m1 -iE "address already in use|bind:|fatal|panic|listen udp|cannot" "$R/edge.log" | cut -c1-200 | tr -d '"\\')
EUP=$(grep -m1 -E "user port .* open" "$R/edge.log" | sed 's/^[0-9/ :.]*//' | tr -d '"\\')
printf '{"case":"%s","topo":"%s","udp":%s,"cond":"%s","bin":"%s","check_edge":%d,"check_exit":%d,"links_up_s":"%s","links":%s,"links_after":%s,"edge_alive":%d,"edge_alive_after":%d,"exit_alive":%d,"tcp":%s,"udpc":%s,"edge_userport":"%s","edge_fatal":"%s"}\n' \
  "$CASE" "$TOPO" "$UDP" "$CONDS" "$(basename "$BIN")" $CE $CX "$UPS" "$L" "$L2" $EALIVE $EALIVE2 $XALIVE "$TC" "$UC" "$EUP" "$FATAL" | tee -a "$OUT/res.jsonl"

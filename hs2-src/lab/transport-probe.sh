#!/usr/bin/env bash
# hs2 transport probe — does each REVERSE transport actually carry data on the
# real path between THIS pair of servers?
#
# Run it on BOTH servers at (roughly) the same time, same PASS, each in its role:
#   iran:   ROLE=iran   PEER=<kharej public ip>  PASS=<shared word>  lab/transport-probe.sh
#   kharej: ROLE=kharej PEER=<iran   public ip>  PASS=<shared word>  lab/transport-probe.sh
#
# It does NOT touch the live tunnel: a separate port, a separate TUN interface
# (hst9), a separate 10.79.0.x subnet and separate helper ports are used, so
# hs2.service keeps serving users throughout. The two sides march through the
# same ordered transport list in lock-step by wall clock (VPS clocks are NTP
# synced), so no link or value needs copying between them — only PASS, which you
# choose and type on both. The IRAN side prints the result table.
#
# Env: ROLE (iran|kharej, required), PEER (required), PASS (required),
#      EGRESS (local IP to dial/bind from, optional), TPORT (tunnel test port,
#      default 3390), SLOT (seconds per transport, default 30), ROUNDS (default 2).
set -u

ROLE=${ROLE:-}; PEER=${PEER:-}; PASS=${PASS:-}
EGRESS=${EGRESS:-}; TPORT=${TPORT:-3390}; SLOT=${SLOT:-35}; ROUNDS=${ROUNDS:-2}
[ "$ROLE" = iran ] || [ "$ROLE" = kharej ] || { echo "set ROLE=iran or ROLE=kharej" >&2; exit 2; }
[ -n "$PEER" ] || { echo "set PEER=<the other server's public IP>" >&2; exit 2; }
[ -n "$PASS" ] || { echo "set PASS=<a shared word, the SAME on both servers>" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }

for t in ip python3; do command -v "$t" >/dev/null || { echo "need '$t' installed" >&2; exit 2; }; done
BIN=${BIN:-}
if [ -z "$BIN" ]; then
  for c in /usr/local/bin/hs2 ./hs2 ./hs2-linux-amd64; do [ -x "$c" ] && { BIN=$c; break; }; done
fi
[ -n "$BIN" ] && [ -x "$BIN" ] || { echo "hs2 binary not found (set BIN=/path/to/hs2)" >&2; exit 2; }

W=$(mktemp -d); IFACE=hst9; ECHOPORT=19000; FWD=18443
IR_TUN=10.79.0.1; KH_TUN=10.79.0.2
KEY=$(printf '%s' "$PASS" | sha256sum | cut -c1-64)   # 32-byte hex, same on both
# self-signed cert for the TLS carriers (auth is by the shared key, not a CA)
CERT="$W/c.pem"; CKEY="$W/k.pem"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -keyout "$CKEY" -out "$CERT" \
  -days 5 -nodes -subj "/CN=probe.local" -addext "subjectAltName=DNS:probe.local" >/dev/null 2>&1

# The ordered transport list both sides walk. tag|carrier|encap|mtu|check
#   check=fwd  -> tested through the forwarded port (mtcp/tls/l3mtcp/dgtun)
#   check=tun  -> tested over the TUN IP directly (udp/auto have no forwarder)
LIST=(
  "auto|auto||1280|tun"
  "udp|udp||1280|tun"
  "mtcp|mtcp||1380|fwd"
  "tls|tls||1380|fwd"
  "tun-udp|dgtun|udp|1280|fwd"
  "tun-icmp|dgtun|icmp|1280|fwd"
  "tun-gre|dgtun|gre|1280|fwd"
  "tun-ipip|dgtun|ipip|1280|fwd"
  "tun-ipx|dgtun|ipx|1280|fwd"
  "tun-tcp|l3mtcp||1320|fwd"
)
N=${#LIST[@]}

HS2PID=""; ECHOPID=""
kill_hs2(){ [ -n "$HS2PID" ] && kill -TERM "$HS2PID" 2>/dev/null; HS2PID=""; sleep 0.6; ip link del "$IFACE" 2>/dev/null; }
cleanup(){ kill_hs2; [ -n "$ECHOPID" ] && kill "$ECHOPID" 2>/dev/null; rm -rf "$W"; }
trap cleanup EXIT INT TERM

# Tiny TCP echo server (kharej). Binds 0.0.0.0:ECHOPORT so it answers both the
# forwarded path (127.0.0.1:ECHOPORT) and the tun path (KH_TUN:ECHOPORT).
start_echo(){
  python3 - "$ECHOPORT" >/dev/null 2>&1 <<'PY' &
import socket,sys,threading
p=int(sys.argv[1]); s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("0.0.0.0",p)); s.listen(64)
def h(c):
    try:
        while True:
            d=c.recv(65536)
            if not d: break
            c.sendall(d)
    except Exception: pass
    finally: c.close()
while True:
    try: c,_=s.accept()
    except Exception: break
    threading.Thread(target=h,args=(c,),daemon=True).start()
PY
  ECHOPID=$!
}

# Client (iran): keep trying to connect for up to $2 seconds (transports differ
# in warm-up: a TLS pool + tun is slower than a bare UDP carrier), then push data
# for ~5s and verify it echoes back. Prints PASS:<Mbit/s> or FAIL:<reason>.
# Arg1 = host:port, Arg2 = seconds to keep retrying the connect.
probe_once(){
python3 - "$1" "${2:-8}" <<'PY'
import socket,sys,time
hostport=sys.argv[1]; host,port=hostport.rsplit(":",1); port=int(port)
retry_until=time.time()+float(sys.argv[2])
c=None; lasterr="no-connect"
while time.time()<retry_until:
    try:
        c=socket.create_connection((host,port),timeout=3)
        c.settimeout(5)
        c.sendall(b"hs2probe")
        r=c.recv(8)
        if r==b"hs2probe": break
        lasterr="bad-echo"; c.close(); c=None
    except Exception:
        lasterr="no-connect"
        if c:
            try: c.close()
            except Exception: pass
        c=None
        time.sleep(1)
if c is None:
    print("FAIL:%s"%lasterr); sys.exit(0)
import threading
got=[0]
dur=5.0; end=time.time()+dur
def reader():
    try:
        while time.time()<end+2:
            d=c.recv(65536)
            if not d: break
            got[0]+=len(d)
    except Exception: pass
t=threading.Thread(target=reader,daemon=True); t.start()
buf=b"x"*32768
try:
    while time.time()<end: c.sendall(buf)
except Exception: pass
t.join(2); c.close()
if got[0]<=0: print("FAIL:no-data"); sys.exit(0)
print("PASS:%.1f"%(got[0]*8/1e6/dur))
PY
}

# Write this role's config for one transport into $W/cfg.json.
write_cfg(){ # carrier encap mtu check
  local carrier=$1 encap=$2 mtu=$3 check=$4
  local encline=""; [ -n "$encap" ] && encline="\"encap\": \"$encap\","
  local proto=""; [ "$encap" = ipx ] && proto="\"proto\": 253,"
  local pool="\"min_links\": 2, \"max_links\": 8, \"per_link\": 8"
  local tls=""; case "$carrier" in mtcp|tls|l3mtcp) tls=1;; esac
  if [ "$ROLE" = iran ]; then
    # edge, reverse: LISTENS on 0.0.0.0:TPORT. TLS carriers need the cert here.
    local fwd=""; [ "$check" = fwd ] && fwd="\"forward_ports\": \"$FWD\", \"user_listen_ip\": \"127.0.0.1\","
    local certline=""; [ -n "$tls" ] && certline="\"backend_addr\": \"builtin\", \"cert_file\": \"$CERT\", \"key_file\": \"$CKEY\","
    cat > "$W/cfg.json" <<J
{ "mode": "dial", "carrier": "$carrier", $encline "reverse": true, "udp": false,
  "addr": "0.0.0.0:$TPORT", $proto
  "iface": "$IFACE", "local_cidr": "$IR_TUN/30", "peer_ip": "$KH_TUN", "mtu": $mtu,
  "shared_key": "$KEY", $certline $fwd $pool }
J
  else
    # exit, reverse: DIALS the iran edge. Has the panel (echo) via expose.
    local fwd=""; [ "$check" = fwd ] && fwd="\"expose\": \"127.0.0.1:$ECHOPORT\", \"forward_ports\": \"$FWD\","
    local egress=""; [ -n "$EGRESS" ] && egress="\"bind_local_ip\": \"$EGRESS\","
    local sni=""; [ -n "$tls" ] && sni="\"sni\": \"probe.local\","
    cat > "$W/cfg.json" <<J
{ "mode": "listen", "carrier": "$carrier", $encline "reverse": true,
  "addr": "$PEER:$TPORT", $sni $proto
  "iface": "$IFACE", "local_cidr": "$KH_TUN/30", "peer_ip": "$IR_TUN", "mtu": $mtu,
  "shared_key": "$KEY", $fwd $egress $pool }
J
  fi
}

start_transport(){ # index
  IFS='|' read -r tag carrier encap mtu check <<< "${LIST[$1]}"
  write_cfg "$carrier" "$encap" "$mtu" "$check"
  HS2_NO_TUNE=1 "$BIN" run -c "$W/cfg.json" >"$W/hs2.log" 2>&1 &
  HS2PID=$!
}

declare -A RESULT
[ "$ROLE" = kharej ] && start_echo

echo "probe: role=$ROLE peer=$PEER port=$TPORT slot=${SLOT}s rounds=$ROUNDS ($((N*SLOT*ROUNDS/60))m). Not touching the live tunnel." >&2

cur=-1; probed=-1; deadline=$(( $(date +%s) + N*SLOT*ROUNDS + 5 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  now=$(date +%s); slot=$(( now / SLOT )); idx=$(( slot % N ))
  if [ "$idx" != "$cur" ]; then
    kill_hs2; cur=$idx
    start_transport "$idx"
  fi
  if [ "$ROLE" = iran ] && [ "$slot" != "$probed" ] && [ $(( now % SLOT )) -ge $(( SLOT/3 )) ]; then
    # Probe this slot once, starting a third of the way in and retrying the
    # connect for the rest of the slot (slower transports — a TLS pool + tun —
    # need more warm-up than a bare UDP carrier). Keep the best result per
    # transport across rounds (PASS beats FAIL, higher Mbit/s wins).
    IFS='|' read -r tag carrier encap mtu check <<< "${LIST[$idx]}"
    target="127.0.0.1:$FWD"; [ "$check" = tun ] && target="$KH_TUN:$ECHOPORT"
    budget=$(( SLOT - (now % SLOT) - 6 )); [ "$budget" -lt 4 ] && budget=4
    r=$(probe_once "$target" "$budget"); probed=$slot
    prev=${RESULT[$tag]:-}
    case "$prev" in
      "" ) RESULT[$tag]=$r;;
      FAIL:* ) [ "${r%%:*}" = PASS ] && RESULT[$tag]=$r;;
      PASS:* ) if [ "${r%%:*}" = PASS ]; then
                 awk "BEGIN{exit !(${r#PASS:}>${prev#PASS:})}" && RESULT[$tag]=$r
               fi;;
    esac
  fi
  sleep 2
done
kill_hs2

if [ "$ROLE" = kharej ]; then
  echo "kharej: finished cycling all transports. The result table is on the IRAN side." >&2
  exit 0
fi

# IRAN prints the table.
echo
printf '%-10s  %-6s  %s\n' "TRANSPORT" "DATA" "DETAIL"
printf '%-10s  %-6s  %s\n' "---------" "----" "------"
pass=0
for entry in "${LIST[@]}"; do
  IFS='|' read -r tag carrier encap mtu check <<< "$entry"
  r=${RESULT[$tag]:-FAIL:not-tested}
  if [ "${r%%:*}" = PASS ]; then
    printf '%-10s  %-6s  %s Mbit/s\n' "$tag" "yes" "${r#PASS:}"; pass=$((pass+1))
  else
    reason=${r#FAIL:}
    case "$reason" in
      no-connect) reason="tunnel never came up (this transport is blocked or not reachable)";;
      no-data)    reason="connected but no data crossed";;
      bad-echo)   reason="data corrupted";;
      not-tested) reason="not tested (missed its slot; try more ROUNDS)";;
    esac
    printf '%-10s  %-6s  %s\n' "$tag" "NO" "$reason"
  fi
done
echo
echo "$pass of $N transports carried data. Use one of the 'yes' transports on the reverse setup."

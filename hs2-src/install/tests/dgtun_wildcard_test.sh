#!/bin/bash
# Regression test for the datagram-tun panel-inbound bind conflict.
#
# A panel (x-ui/marzban) binds the user port on 0.0.0.0. The dgtun EXIT
# forwarder used to listen on <tun_ip>:<user_port>, which on Linux fails with
# "bind: address already in use" against that wildcard even with SO_REUSEADDR, so
# hs2 crash-looped on the kharej. The forwarder now uses a shifted, tunnel-
# private on-tun port (engine.tunForwardPort), so the exit listens fine and a
# user on iran:<port> still reaches the panel.
#
# This runs a REAL reverse dgtun (encap ipx) in two network namespaces with a
# panel on 0.0.0.0:<port> and forward port == panel port, and asserts the exit
# does not crash and traffic flows. Needs root + iproute2 + go (or HS2_BIN).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); SRC=$(cd "$HERE/../.." && pwd)
PORT=8443; IR=dgwcir; KH=dgwckh
fail=0; ok(){ echo "PASS $1"; }; bad(){ echo "FAIL $1"; fail=1; }

if [ "$(id -u)" != 0 ] || ! command -v ip >/dev/null; then
  echo "SKIP dgtun_wildcard_test (needs root and iproute2)"; exit 0
fi

W=$(mktemp -d); : > "$W/pids"
cleanup(){ for p in $(cat "$W/pids" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  ip netns del $IR 2>/dev/null; ip netns del $KH 2>/dev/null; rm -rf "$W"; }
trap cleanup EXIT

BIN=${HS2_BIN:-}; PROBE=${PROBE_BIN:-}
[ -n "$BIN" ]   || { BIN="$W/hs2";   (cd "$SRC" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/hs2) || { bad "build hs2"; exit 1; }; }
[ -n "$PROBE" ] || { PROBE="$W/probe"; (cd "$SRC" && go build -o "$PROBE" ./lab/probe) || { bad "build probe"; exit 1; }; }

ip netns add $IR && ip netns add $KH
ip link add dgwcvi netns $IR type veth peer name dgwcvk netns $KH
ip -n $IR addr add 192.168.72.1/24 dev dgwcvi
ip -n $KH addr add 192.168.72.2/24 dev dgwcvk
for n in $IR $KH; do ip -n $n link set lo up; done
ip -n $IR link set dgwcvi up; ip -n $KH link set dgwcvk up

# Panel on the kharej, bound on the WILDCARD at the user port.
ip netns exec $KH "$PROBE" -server -listen 0.0.0.0:$PORT >"$W/panel.log" 2>&1 & echo $! >> "$W/pids"

KEY=$(printf 'cd%.0s' {1..32})
cat > "$W/ir.json" <<J
{"mode":"dial","carrier":"dgtun","encap":"ipx","reverse":true,"addr":"192.168.72.1:2082",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1280,"shared_key":"$KEY",
 "forward_ports":"$PORT","user_listen_ip":"","min_links":2,"max_links":8,"per_link":8}
J
cat > "$W/kh.json" <<J
{"mode":"listen","carrier":"dgtun","encap":"ipx","reverse":true,"addr":"192.168.72.1:2082",
 "iface":"hs0","local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1280,"shared_key":"$KEY",
 "expose":"127.0.0.1:$PORT","forward_ports":"$PORT","min_links":2,"max_links":8,"per_link":8}
J

ip netns exec $IR env HS2_NO_TUNE=1 "$BIN" run -c "$W/ir.json" >"$W/ir.log" 2>&1 & echo $! >> "$W/pids"
sleep 1
ip netns exec $KH env HS2_NO_TUNE=1 "$BIN" run -c "$W/kh.json" >"$W/kh.log" 2>&1 & echo $! >> "$W/pids"
sleep 6

if grep -q 'address already in use' "$W/kh.log"; then
  bad "exit crash-looped on the wildcard panel: $(grep -m1 'address already in use' "$W/kh.log")"
else
  ok "exit started despite a panel on 0.0.0.0:$PORT (no bind conflict)"
fi

up=0
for i in $(seq 1 40); do
  ip netns exec $IR bash -c "exec 3<>/dev/tcp/127.0.0.1/$PORT" 2>/dev/null && { up=1; break; }
  sleep 0.25
done
[ "$up" = 1 ] && ok "iran:$PORT accepts user connections" || bad "iran:$PORT never accepted"

if [ "$up" = 1 ]; then
  r=$(ip netns exec $IR "$PROBE" -addr 127.0.0.1:$PORT -bulk 2 -t 3s -warm 1s 2>/dev/null | tail -1)
  if echo "$r" | grep -q '"bulk_errors":0' && echo "$r" | grep -q '"conn_fail":0' && echo "$r" | grep -q '"echo_lost":0'; then
    ok "a user on iran:$PORT reaches the panel through dgtun ($(echo "$r" | grep -oE '"mbps":[0-9.]+'))"
  else
    bad "traffic did not flow cleanly: $r"
  fi
fi

[ "$fail" = 0 ] && echo "OK: dgtun wildcard-panel regression passed" || { echo "--- kharej log ---"; tail -20 "$W/kh.log"; }
exit $fail

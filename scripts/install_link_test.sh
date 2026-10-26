#!/bin/bash
# C15 (docs/architecture/REDESIGN.fa.md §3.16) in the installer, driven on a pty
# under the installer's real `set -euo pipefail`, with the REAL hs2 binary built
# from this tree. Everything runs in a sandbox: the installer's paths (configs,
# units, binary, backups) are rewritten into a temp directory and systemctl /
# journalctl are stand-ins, so no real tunnel, config or unit is read or touched.
#
#  1. every way setup makes a link (tcp with mtcp/l3mtcp/tls, tun over TLS with
#     l3mtcp/tls, tun over udp/icmp/gre/ipip/ipx, udp, auto; direct and
#     reverse): the link printed at the end is the one `hs2 link` builds from the
#     config just written, byte-for-byte what the bash encoder makes of the same
#     answers; pasted on the other side, THAT side's config rebuilds the very
#     same link;
#  2. an older binary without `hs2 link`: setup falls back to the bash encoder
#     and the paste to the bash decoder — the same link, the same configs;
#  3. an hs2v3:// link is refused with "update the installer"; nothing written;
#  4. the menu "Show a tunnel's hs2:// link": list → pick → the missing fields
#     asked with this server's defaults → the link → saved on request into the
#     config's link block (0600, .prev kept) → shown again with no question;
#     from the main menu (9) and a tunnel's menu (l); a QR code with qrencode,
#     a hint without it; an older binary is refused with the reason;
#  5. without root: the installer and the screen refuse with a clear message,
#     and `hs2 link` exits 4 saying root is needed.
#
#   bash scripts/install_link_test.sh      (exit 0 = all PASS; needs root, go, script)
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); ROOT=$(cd "$HERE/.." && pwd); INST="$ROOT/install.sh"
# Inside hs2-src/install/tests/sandbox.sh (its own PID and mount namespaces,
# checked first), or not at all.
case "${HS2_SANDBOX:-}" in
  proven|nonroot) ;;
  *) exec bash "$ROOT/hs2-src/install/tests/sandbox.sh" bash "$0" "$@" ;;
esac
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
chmod 755 "$T"
fail=0; out=""
check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-900)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP install_link_test (no script(1))"; exit 0; }
GO=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$GO" ] || { echo "SKIP install_link_test (no go toolchain to build hs2)"; exit 0; }
( cd "$ROOT/hs2-src" && CGO_ENABLED=0 "$GO" build -o "$T/hs2" ./cmd/hs2 ) || { echo "FAIL build hs2"; exit 1; }
chmod 755 "$T/hs2"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$T/k.pem" -out "$T/c.pem" \
  -days 30 -subj /CN=tunnel.example.com -addext subjectAltName=DNS:tunnel.example.com >/dev/null 2>&1 \
  || { echo "FAIL openssl cert"; exit 1; }
chmod 644 "$T/c.pem" "$T/k.pem"
# An older binary: everything as the real one, except it has no `link`.
cat > "$T/hs2old" <<EOF
#!/bin/bash
[ "\${1:-}" = link ] && { echo "unknown command"; exit 2; }
exec "$T/hs2" "\$@"
EOF
chmod 755 "$T/hs2old"

# The installer up to its command dispatch, plus the main menu, with every path
# it owns moved into the sandbox $SBX and the root check left to section 5.
{ awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST"; sed -n '/^main_menu(){/,/^}/p' "$INST"; } \
  | sed -e 's#^BIN=/usr/local/bin/hs2$#BIN="$SBX/bin/hs2"#' \
        -e 's#^CFG_DIR=/etc/hs2$#CFG_DIR="$SBX/etc-hs2"#' \
        -e 's#^UNIT_DIR=/etc/systemd/system$#UNIT_DIR="$SBX/units"#' \
        -e 's#^MENU_BIN=.*#MENU_BIN="$SBX/bin/hs2-menu"#' \
        -e 's#^SYSCTL_CONF=.*#SYSCTL_CONF="$SBX/sysctl-99-hs2.conf"#' \
        -e 's#^MODULES_CONF=.*#MODULES_CONF="$SBX/modules-hs2.conf"#' \
        -e 's#^SAFE_APPLY_BIN=.*#SAFE_APPLY_BIN="$SBX/bin/hs2-safe-apply"#' \
        -e 's#^BACKUP_DIR=.*#BACKUP_DIR="$SBX/backups"#' \
        -e 's#^\[ "\$(id -u)" = 0 \] || die .*#:#' > "$T/core.sh"
for want in 'BIN="$SBX/bin/hs2"' 'CFG_DIR="$SBX/etc-hs2"' 'UNIT_DIR="$SBX/units"' 'BACKUP_DIR="$SBX/backups"' 'main_menu(){' \
            'SYSCTL_CONF="$SBX/sysctl-99-hs2.conf"' 'MODULES_CONF="$SBX/modules-hs2.conf"' 'SAFE_APPLY_BIN="$SBX/bin/hs2-safe-apply"' 'MENU_BIN="$SBX/bin/hs2-menu"'; do
  grep -qxF "$want" "$T/core.sh" || grep -qF "$want" "$T/core.sh" || { echo "FAIL sandbox: $want not found in install.sh"; exit 1; }
done
grep -q '^\[ "\$(id -u)" = 0 \]' "$T/core.sh" && { echo "FAIL sandbox: root check still there"; exit 1; }
chmod 644 "$T/core.sh"

cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
say(){ printf '%s\n' "$*"; }; warn(){ say "WARN $*"; }; info(){ say "INFO $*"; }; ok(){ say "OK $*"; }; err(){ say "ERR $*"; }
hr(){ :; }; pause(){ :; }
systemctl(){ case "${1:-}" in is-active) echo inactive ;; is-enabled) echo disabled ;; esac; return 0; }
journalctl(){ :; }
local_ips(){ printf '192.0.2.10\n'; }
ask_subnet(){ set_tun_subnet 10.77.4.0; }
free_iface(){ echo hs9; }
ask_tunnel_port(){ if raw_encap; then TPORT=""; PSUF=""; else TPORT=2096; PSUF=":2096"; fi; }
ask_bind_ip(){ BINDADDR=0.0.0.0; }
ask_kharej_targets(){ PANEL=127.0.0.1:8443; PMJ=""; }
ask_domain(){ DOMAIN=tunnel.example.com; }
get_cert(){ printf '%s|%s' "$T/c.pem" "$T/k.pem"; }
ask_tun_params(){ TUNIF=hs9; }
ask_tun_mtu(){ TUNMTU=1320; }
ask_user_ip(){ USERIP=""; }
ask_egress_ip(){ EGRESSIP=""; }
write_service(){ :; }; start_service(){ :; }; tunnel_summary(){ :; }
port_free(){ true; }; udp_port_free(){ true; }; used_blocks(){ :; }
LINK_MIN=2; LINK_MAX=8; LINK_PER=8
eval "$CALL"
echo "RETURNED"
INNER
chmod 644 "$T/inner.sh"

# sandbox DIR [BIN]: a fresh installer root.
sandbox(){ mkdir -p "$1/bin" "$1/etc-hs2" "$1/units" "$1/backups"; cp "${2:-$T/hs2}" "$1/bin/hs2"; chmod 755 "$1" "$1/bin" "$1/bin/hs2"; }
# run SBX CALL KEYS [PATH-PREFIX]: the installer function(s) on a pty, KEYS typed.
run(){
  printf '%b' "$3" | env T="$T" SBX="$1" CALL="$2" PATH="${4:+$4:}$PATH" script -qec "bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
linkof(){ printf '%s' "$1" | { grep -o 'hs2\(v3\)\{0,1\}://[A-Za-z0-9+/=]\{1,\}' || true; } | tail -1; }
keyof(){ sed -n 's/.*"shared_key": *"\([0-9a-f]*\)".*/\1/p' "$1" | head -1; }

# ---- 1 + 2. setup makes the link from the config; the other side rebuilds it --
n=0
roundtrip(){ # transport carrier encap proto udp direction [binary]
  local tr=$1 car=$2 enc=$3 proto=$4 udp=$5 dir=$6 bin=${7:-$T/hs2}
  local name="$dir $tr/$car${enc:+/$enc}${proto}$( [ "$udp" = true ] && echo +udp)$( [ "$bin" = "$T/hs2old" ] && echo ' (older binary)')"
  n=$((n + 1))
  local mk="$T/mk$n" pa="$T/pa$n" fn pub keys m u cfg pcfg shared psuf dom uf pan mtu en pr want link
  sandbox "$mk" "$bin"; sandbox "$pa" "$bin"
  if [ "$dir" = direct ]; then fn=kharej_listener; pub=192.0.2.10; else fn=iran_listener; pub=198.51.100.20; fi
  case $car in mtcp) m=1 ;; l3mtcp) m=2 ;; tls) m=3 ;; *) m="" ;; esac
  u=n; [ "$udp" = true ] && u=y
  keys=""
  case "$dir/$tr" in
    direct/tcp) keys="$m\n$u\n" ;;
    direct/tun) [ "$enc" = tcp ] && keys="$u\n" ;;
    reverse/tcp) keys="8443\n$m\n$u\n" ;;
    reverse/tun) if [ "$enc" = tcp ]; then keys="8443\n$u\n"; else keys="8443\n"; fi ;;
  esac
  out=$(run "$mk" "use_unit hs2-lt1; TRANSPORT=$tr; TUN_ENCAP=${enc:-udp}; TUN_PROTO=$proto; CARRIER=$car; PUBIP=$pub; $fn" "$keys")
  link=$(linkof "$out")
  cfg="$mk/etc-hs2/hs2-lt1.json"
  shared=$(keyof "$cfg")
  psuf=":2096"; case $enc in icmp|gre|ipip|ipx) psuf="" ;; esac
  dom="-"; uf=false; pan="-"; mtu=""; en=""; pr=""
  case $car in mtcp|l3mtcp|tls) dom=tunnel.example.com; uf=$udp ;; esac
  if [ "$dir" = direct ]; then case $car in udp|auto) ;; *) pan=127.0.0.1:8443 ;; esac; fi
  if [ "$tr" = tun ]; then en=$enc; if [ "$enc" = tcp ]; then mtu=1320; else mtu=1280; fi; [ "$enc" = ipx ] && pr=${proto:-253}; fi
  want="hs2://$(printf '%s' "$pub$psuf|$dom|$shared|$pan|$car|$uf|$tr|$dir|$mtu|$en|$pr|hs2-lt1|10.77.4.0" | base64 -w0)"
  check "$name: setup prints the link, byte-for-byte what the bash encoder makes of the same answers" \
    '[ -n "$shared" ] && [ "$link" = "$want" ] && echo "$out" | grep -q RETURNED'
  check "$name: setup says the link holds the key and what to do if it leaks" \
    'echo "$out" | grep -q "keep it private (if it leaks, reinstall the tunnel on both servers; hs2 rekey arrives in a later release)"'
  check "$name: the config keeps the link block" \
    '[ "$("$T/hs2" config -c "$cfg" get link.endpoint)" = "$pub$psuf" ] && [ "$("$T/hs2" config -c "$cfg" get link.transport)" = "$tr" ]'
  check "$name: hs2 link -c rebuilds it on the side that made it" \
    '[ "$("$T/hs2" link -c "$cfg" --unit hs2-lt1 2>/dev/null)" = "$want" ]'
  if [ "$dir" = direct ]; then fn=iran_dialer; else fn=kharej_dialer; fi
  keys="$link\n"
  if [ "$fn" = iran_dialer ]; then case $tr in tcp|tun) keys="${keys}8443\n" ;; esac; fi
  out=$(run "$pa" "$fn" "$keys")
  pcfg="$pa/etc-hs2/hs2-lt1.json"
  check "$name: pasted on the other side, its config rebuilds the very same link" \
    '[ -f "$pcfg" ] && echo "$out" | grep -q RETURNED && [ "$("$T/hs2" link -c "$pcfg" --unit hs2-lt1 2>/dev/null)" = "$want" ]'
}
for dir in direct reverse; do
  for car in mtcp l3mtcp tls; do for udp in false true; do roundtrip tcp $car "" "" $udp $dir; done; done
  for car in l3mtcp tls; do for udp in false true; do roundtrip tun $car tcp "" $udp $dir; done; done
  for enc in udp icmp gre ipip; do roundtrip tun dgtun $enc "" false $dir; done
  roundtrip tun dgtun ipx 253 false $dir
  roundtrip tun dgtun ipx 200 false $dir
  roundtrip udp udp "" "" false $dir
  roundtrip auto auto "" "" false $dir
done
# the new path really is the binary: setup asks `hs2 link -c`, the paste `hs2 link --decode` (key on stdin)
cat > "$T/hs2spy" <<EOF
#!/bin/bash
[ "\${1:-}" = link ] && printf '%s\n' "\$*" >> "$T/spy.log"
exec "$T/hs2" "\$@"
EOF
chmod 755 "$T/hs2spy"
roundtrip tun tls tcp "" true direct "$T/hs2spy"
out=$(cat "$T/spy.log" 2>/dev/null)
check "setup builds the link with 'hs2 link -c CFG --unit NAME', the paste reads it with 'hs2 link --decode' (no link on the command line)" \
  'grep -q "^link -c .*/etc-hs2/hs2-lt1.json --unit hs2-lt1$" "$T/spy.log" && grep -qx "link --decode" "$T/spy.log" && ! grep -q "hs2://" "$T/spy.log"'
roundtrip tcp mtcp "" "" true direct "$T/hs2old"
roundtrip tun dgtun ipx 200 false reverse "$T/hs2old"
roundtrip tun l3mtcp tcp "" true reverse "$T/hs2old"

# ---- 3. an extended hs2v3:// link (R8/R9) is refused with the reason ----------
sandbox "$T/v3"
v3="hs2v3://$(printf '%s' "192.0.2.10:2096|-|$(printf 'ab%.0s' $(seq 32))|-|dgtun|false|tun|direct|1280|udp||hs2-lt1|10.77.4.0|203.0.113.7:2096|" | base64 -w0)"
out=$(run "$T/v3" "iran_dialer" "$v3\n8443\n")
check "an hs2v3:// link: 'update the installer', and nothing is written" \
  'echo "$out" | grep -q "newer installer" && ! echo "$out" | grep -q RETURNED && [ -z "$(ls "$T/v3/etc-hs2")" ]'
out=$(run "$T/v3" "iran_dialer" "hs2://not-a-link\n8443\n")
check "a broken link is refused as invalid, nothing written" \
  'echo "$out" | grep -qi "invalid link" && ! echo "$out" | grep -q RETURNED && [ -z "$(ls "$T/v3/etc-hs2")" ]'

# ---- 4. the menu: Show a tunnel's hs2:// link ------------------------------------
M="$T/menu"; sandbox "$M"
KEY=$(printf 'cd%.0s' $(seq 32))
# hs2-lt1: a pre-R1 exit of a direct tcp tunnel (no link block): needs endpoint and udp.
cat > "$M/etc-hs2/hs2-lt1.json" <<EOF
{"mode": "listen", "carrier": "mtcp", "reverse": false, "addr": "0.0.0.0:2096", "iface": "hs9",
 "local_cidr": "10.77.4.2/30", "peer_ip": "10.77.4.1", "mtu": 1380, "backend_addr": "builtin",
 "shared_key": "$KEY", "cert_file": "$T/c.pem", "key_file": "$T/k.pem", "expose": "127.0.0.1:8443"}
EOF
# hs2-lt2: a pre-R1 edge of a direct tun-over-TLS tunnel: needs transport (l3mtcp is tcp or tun).
cat > "$M/etc-hs2/hs2-lt2.json" <<EOF
{"mode": "dial", "carrier": "l3mtcp", "reverse": false, "udp": false, "addr": "192.0.2.10:2097",
 "sni": "tunnel.example.com", "iface": "hs8", "local_cidr": "10.77.8.1/30", "peer_ip": "10.77.8.2", "mtu": 1320,
 "shared_key": "$KEY", "forward_ports": "8443", "peer_panel": "127.0.0.1:8443", "user_listen_ip": "",
 "min_links": 2, "max_links": 8, "per_link": 8, "bind_local_ip": ""}
EOF
chmod 600 "$M/etc-hs2/"*.json
for u in hs2-lt1 hs2-lt2; do printf '[Service]\nExecStart=%s run -c %s\n' "$M/bin/hs2" "$M/etc-hs2/$u.json" > "$M/units/$u.service"; done
# hs2-* units that are NOT tunnels (a monitor, hs2 run on a file that is no
# tunnel config, another program given -c): never listed as tunnels
printf '[Service]\nExecStart=/usr/local/sbin/hs2-mon\n' > "$M/units/hs2-mon.service"
printf '{"foo": 1}\n' > "$M/etc-hs2/junk.json"
printf '[Service]\nExecStart=%s run -c %s\n' "$M/bin/hs2" "$M/etc-hs2/junk.json" > "$M/units/hs2-junk.service"
printf '[Service]\nExecStart=/usr/bin/python3 /opt/x.py -c %s\n' "$M/etc-hs2/hs2-lt1.json" > "$M/units/hs2-aaa.service"
# a masked tunnel (a /dev/null link) whose config is still there counts; one without a config does not
ln -s /dev/null "$M/units/hs2-lt9.service"; cp "$M/etc-hs2/hs2-lt2.json" "$M/etc-hs2/hs2-lt9.json"
ln -s /dev/null "$M/units/hs2-gone.service"
out=$(run "$M" 'echo "UNITS=[$(tm_units | tr "\n" " ")]"' "")
check "tm_units: only the tunnels (hs2 run -c <tunnel config>, or masked with its config), not hs2-mon and the like" \
  'echo "$out" | grep -qx "UNITS=\[hs2-lt1 hs2-lt2 hs2-lt9 \]"'
rm -f "$M/units/hs2-lt9.service" "$M/etc-hs2/hs2-lt9.json"
orig1=$(cat "$M/etc-hs2/hs2-lt1.json"); orig2=$(cat "$M/etc-hs2/hs2-lt2.json")
want1="hs2://$(printf '%s' "192.0.2.10:2096|tunnel.example.com|$KEY|127.0.0.1:8443|mtcp|true|tcp|direct||||hs2-lt1|10.77.4.0" | base64 -w0)"
want2="hs2://$(printf '%s' "192.0.2.10:2097|tunnel.example.com|$KEY|127.0.0.1:8443|l3mtcp|false|tun|direct|1320|tcp||hs2-lt2|10.77.8.0" | base64 -w0)"

# pick 1 → endpoint (Enter = this server's first address + the listen port) → udp yes → save yes
out=$(run "$M" "show_tunnel_link" "1\n\ny\ny\n")
check "menu: lists the tunnels, and only them" \
  'echo "$out" | grep -q "1) hs2-lt1" && echo "$out" | grep -q "2) hs2-lt2" && ! echo "$out" | grep -q "hs2-mon\|hs2-junk\|hs2-aaa\|hs2-gone"'
out2=$(run "$M" "tunnel_manager" "0\n")
check "tunnel manager: lists only the tunnels" \
  'echo "$out2" | grep -q "1) .*hs2-lt1" && ! echo "$out2" | grep -q "hs2-mon\|hs2-junk\|hs2-aaa\|hs2-gone"'
check "menu: asks the endpoint with this server's address and the listen port as the default" 'echo "$out" | grep -q "\[192.0.2.10:2096\]"'
check "menu: asks whether the Iran server forwards UDP" 'echo "$out" | grep -qi "forward.* UDP"'
check "menu: shows the rebuilt link" '[ "$(linkof "$out")" = "$want1" ]'
check "menu: warns that the link holds the key, and says what to do if it leaks (no command that does not exist yet)" \
  'echo "$out" | grep -q "contains the tunnel key" && echo "$out" | grep -q "if it leaks, reinstall the tunnel on both servers" && echo "$out" | grep -q "hs2 rekey arrives in a later release" && ! echo "$out" | grep -qi "revoke with hs2 rekey\|run hs2 rekey"'
check "menu: no qrencode → a hint, not an error" 'command -v qrencode >/dev/null || echo "$out" | grep -q "qrencode"'
c1="$M/etc-hs2/hs2-lt1.json"
check "menu save: the asked fields go into the config's link block" \
  '[ "$("$T/hs2" config -c "$c1" get link.endpoint)" = 192.0.2.10:2096 ] && [ "$("$T/hs2" config -c "$c1" get link.udp)" = true ]'
check "menu save: the previous config is kept as .prev, both 0600" \
  '[ "$(cat "$c1.prev")" = "$orig1" ] && [ "$(stat -c %a "$c1")" = 600 ] && [ "$(stat -c %a "$c1.prev")" = 600 ]'
check "menu save: the rest of the config is unchanged" \
  '[ "$("$T/hs2" config -c "$c1" get expose)" = 127.0.0.1:8443 ] && [ "$(keyof "$c1")" = "$KEY" ] && "$T/hs2" check -c "$c1" >/dev/null'
check "menu save: no file holds the link" '! grep -rqF "$want1" "$M" "$T/core.sh"'
out=$(run "$M" "show_tunnel_link" "1\n")
check "menu again: nothing is asked, the same link" \
  '[ "$(linkof "$out")" = "$want1" ] && ! echo "$out" | grep -q "Public address" && ! echo "$out" | grep -qi "Save"'
# with qrencode: the QR code is drawn from the link (on stdin, never on a command line)
mkdir -p "$T/qr"; printf '#!/bin/bash\necho "FAKEQR args=[$*] stdin=$(cat)"\n' > "$T/qr/qrencode"; chmod 755 "$T/qr/qrencode"
out=$(run "$M" "show_tunnel_link hs2-lt1" "" "$T/qr")
check "menu with qrencode: a QR code of the link, which goes in on stdin" \
  'echo "$out" | grep -qF "FAKEQR args=[-t ANSIUTF8] stdin=$want1"'
# a tunnel's own menu: l) → transport asked (default from the MTU: tun) → not saved
out=$(run "$M" "tm_tunnel_menu hs2-lt2" "l\n\nn\n0\n")
check "tunnel menu: has l) Show the hs2:// link" 'echo "$out" | grep -q "l) Show the hs2:// link"'
check "tunnel menu l: asks the transport, explains it, default tun" 'echo "$out" | grep -q "tcp or tun" && echo "$out" | grep -q "\[2\]"'
check "tunnel menu l: the link" '[ "$(linkof "$out")" = "$want2" ]'
check "tunnel menu l: not saved when declined (config untouched, no .prev)" \
  '[ "$(cat "$M/etc-hs2/hs2-lt2.json")" = "$orig2" ] && [ ! -e "$M/etc-hs2/hs2-lt2.json.prev" ]'
# the main menu
out=$(run "$M" "main_menu" "9\n1\n0\n")
check "main menu: 9) Show a tunnel's hs2:// link → the link" \
  'echo "$out" | grep -q "9) Show a tunnel'"'"'s hs2:// link" && [ "$(linkof "$out")" = "$want1" ]'
# a stored public address that is no longer on this server: shown, with a warning
cat > "$M/etc-hs2/hs2-lt3.json" <<EOF
{"mode": "listen", "carrier": "dgtun", "encap": "udp", "reverse": false, "addr": "0.0.0.0:2098", "iface": "hs7",
 "local_cidr": "10.77.12.2/30", "peer_ip": "10.77.12.1", "mtu": 1280, "shared_key": "$KEY", "expose": "127.0.0.1:8443",
 "link": {"endpoint": "203.0.113.50:2098", "domain": "-", "transport": "tun", "udp": false, "panel": "127.0.0.1:8443"}}
EOF
chmod 600 "$M/etc-hs2/hs2-lt3.json"
printf '[Service]\nExecStart=%s run -c %s\n' "$M/bin/hs2" "$M/etc-hs2/hs2-lt3.json" > "$M/units/hs2-lt3.service"
want3="hs2://$(printf '%s' "203.0.113.50:2098|-|$KEY|127.0.0.1:8443|dgtun|false|tun|direct|1280|udp||hs2-lt3|10.77.12.0" | base64 -w0)"
out=$(run "$M" "show_tunnel_link hs2-lt3" "")
check "menu: a stored address no longer on this server — the link, and a warning about it" \
  '[ "$(linkof "$out")" = "$want3" ] && echo "$out" | grep -q "203.0.113.50 is not on this server" && ! echo "$out" | grep -q "Public address"'
# an older binary: refused with the reason
O="$T/old"; sandbox "$O" "$T/hs2old"; cp -p "$M/etc-hs2/hs2-lt2.json" "$O/etc-hs2/"
printf '[Service]\nExecStart=%s run -c %s\n' "$O/bin/hs2" "$O/etc-hs2/hs2-lt2.json" > "$O/units/hs2-lt2.service"
out=$(run "$O" "show_tunnel_link hs2-lt2" "")
check "menu, older binary: says to upgrade, no link" 'echo "$out" | grep -qi "upgrade" && [ -z "$(linkof "$out")" ] && echo "$out" | grep -q RETURNED'

# ---- 5. without root -------------------------------------------------------------
if command -v setpriv >/dev/null 2>&1 && [ "$(id -u)" = 0 ]; then
  asnobody(){ setpriv --reuid=65534 --regid=65534 --clear-groups "$@"; }
  cp "$INST" "$T/install-copy.sh"; chmod 644 "$T/install-copy.sh"
  out=$(asnobody bash "$T/install-copy.sh" </dev/null 2>&1)
  check "no root: the installer says so" 'echo "$out" | grep -q "Please run as root"'
  chmod -R a+rX "$M/bin" "$M/units"; chmod 711 "$M/etc-hs2" "$M"
  out=$(asnobody env T="$T" SBX="$M" CALL="show_tunnel_link hs2-lt1" bash "$T/inner.sh" </dev/null 2>&1)
  check "no root: the link screen refuses with a clear message, no link" \
    'echo "$out" | grep -q "only root can read" && echo "$out" | grep -q "sudo hs2-menu" && [ -z "$(linkof "$out")" ]'
  out=$(asnobody "$T/hs2" link -c "$M/etc-hs2/hs2-lt1.json" 2>&1); rc=$?
  check "no root: hs2 link exits 4 and says root is needed" '[ "$rc" = 4 ] && echo "$out" | grep -q "root" && [ -z "$(linkof "$out")" ]'
else
  echo "SKIP no-root checks (need root and setpriv)"
fi

echo "$n setups"
exit $fail

#!/bin/bash
# Per-port routing in the installer, driven on a pty under the installer's
# real `set -euo pipefail`, with the REAL hs2 binary built from this tree (so
# every change goes through the binary's own validation):
#   - Tunnel manager → Ports on the Iran server: add/remove user ports (busy,
#     bad, duplicate and tunnel-port refusals), UDP on/off, and what to do on
#     the Kharej server after each change;
#   - the same screen on the Kharej server: own targets (Enter = the same port
#     on 127.0.0.1, a custom host:port, several at once), remove, the default
#     panel (none / unchanged / bad), and what to do on the Iran server;
#   - an older binary and an IP-tunnel transport are refused with the reason;
#   - the setup question (ask_port_map) writes a port_map the binary accepts,
#     re-asks on a bad entry, and is skipped with a note on an older binary;
#   - every kharej setup template carries the port_map fragment.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"; SRC=$(cd "$HERE/../.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-600)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP ports_menu_test (no script(1))"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "SKIP ports_menu_test (no go toolchain to build hs2)"; exit 0; }
( cd "$SRC" && CGO_ENABLED=0 go build -o "$T/hs2" ./cmd/hs2 ) || { echo "FAIL build hs2"; exit 1; }
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$T/k.pem" -out "$T/c.pem" \
  -days 30 -subj /CN=t.example >/dev/null 2>&1 || { echo "FAIL openssl cert"; exit 1; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
printf '#!/bin/bash\necho "unknown command"; exit 2\n' > "$T/hs2old"; chmod +x "$T/hs2old"

KEY=d0e7d7f85bdd3190377cf4b97c876f52f3ca7227d95356918b6194442af56e54
iran(){ # reverse forward_ports [udp]
  printf '{"mode":"dial","carrier":"mtcp","reverse":%s,"addr":"%s","sni":"t.example","shared_key":"%s","forward_ports":"%s","udp":%s,"cert_file":"%s","key_file":"%s"}\n' \
    "$1" "$( [ "$1" = true ] && echo 0.0.0.0:2096 || echo 1.2.3.4:2096)" "$KEY" "$2" "${3:-false}" "$T/c.pem" "$T/k.pem" > "$T/c.json"
}
kharej(){ # expose [port_map]
  printf '{"mode":"listen","carrier":"mtcp","addr":"0.0.0.0:2096","shared_key":"%s","cert_file":"%s","key_file":"%s","expose":"%s"%s}\n' \
    "$KEY" "$T/c.pem" "$T/k.pem" "$1" "$( [ -n "${2:-}" ] && printf ', "port_map": "%s"' "$2")" > "$T/c.json"
}
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh" >/dev/null 2>&1
say(){ printf '%s\n' "$*"; }; warn(){ say "WARN $*"; }; info(){ say "INFO $*"; }; ok(){ say "OK $*"; }; err(){ say "ERR $*"; }
hr(){ :; }; pause(){ :; }
tm_apply_restart(){ echo "APPLIED"; }
port_free(){ [ "$1" != 7777 ]; }; udp_port_free(){ [ "$1" != 7778 ]; }
status_path(){ echo "$T/none.status.json"; }
BIN="$FAKEBIN"
tm_ports hs2-x "$T/c.json"
echo "RETURNED"
INNER
run(){ # keys [bin]
  printf "$1" | script -qec "T='$T' FAKEBIN='${2:-$T/hs2}' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
field(){ "$T/hs2" config -c "$T/c.json" get "$1"; }

# ---- Iran server ------------------------------------------------------------
iran false 8443
out=$(run '1\n2053, 2083\n0\n')
check "iran add: two ports opened" '[ "$(field forward_ports)" = "8443,2053,2083" ] && echo "$out" | grep -q APPLIED'
check "iran add: says what to do on the Kharej server" 'echo "$out" | grep -q "On the Kharej server:.* nothing to do if 2053,2083 should reach its default panel" && echo "$out" | grep -q "p) Ports → 1) and enter the port (Enter = the same port on 127.0.0.1)"'
check "iran add: the table says the ports and that the Kharej server decides" 'echo "$out" | grep -q "8443, 2053, 2083 (tcp) — where each one goes is set on the Kharej server"'
check "iran add: the screen returns" 'echo "$out" | grep -q RETURNED'
iran false 8443
out=$(run '1\n7777\n0\n')
check "iran add: a busy port is refused before anything changes" 'echo "$out" | grep -q "TCP port 7777 is already in use" && [ "$(field forward_ports)" = 8443 ] && ! echo "$out" | grep -q APPLIED'
out=$(run '1\nabc\n0\n')
check "iran add: not a port" 'echo "$out" | grep -q "'"'"'abc'"'"' is not a port" && ! echo "$out" | grep -q APPLIED'
out=$(run '1\n8443\n0\n')
check "iran add: an open port again — the binary refuses, file untouched" 'echo "$out" | grep -q "ERR user port 8443 is already open" && [ "$(field forward_ports)" = 8443 ] && ! echo "$out" | grep -q APPLIED'
iran false 8443
out=$(run '1\n2053,8443\n0\n')
check "iran add: one refused entry undoes the others and says so" '[ "$(field forward_ports)" = 8443 ] && echo "$out" | grep -q "Nothing was changed: 2053 was undone too" && ! echo "$out" | grep -q APPLIED'
rm -f "$T/c.json.prev"; mkdir -p "$T/c.json.prev"
out=$(run '1\n2053\n0\n')
check "iran add: no backup possible -> nothing changed (an old .prev is never restored)" 'echo "$out" | grep -q "Cannot save a backup" && [ "$(field forward_ports)" = 8443 ] && ! echo "$out" | grep -q APPLIED'
rm -rf "$T/c.json.prev"
iran true 8443
out=$(run '1\n2096\n0\n')
check "iran add (reverse): the tunnel's own listen port is refused" 'echo "$out" | grep -q "2096 is this tunnel'"'"'s own port" && [ "$(field forward_ports)" = 8443 ]'
iran false 8443,2053
out=$(run '2\n8443\n0\n')
check "iran remove: closed, and nothing to change on the Kharej server" '[ "$(field forward_ports)" = 2053 ] && echo "$out" | grep -q "nothing has to change. If it has its own target for 8443"'
out=$(run '2\n2053\n0\n')
check "iran remove: the last port of an mtcp tunnel is refused (config untouched)" 'echo "$out" | grep -q "NOT saved" && [ "$(field forward_ports)" = 2053 ] && ! echo "$out" | grep -q APPLIED'
out=$(run '3\n0\n')
check "iran udp: turned on, Kharej needs nothing" '[ "$(field udp)" = true ] && echo "$out" | grep -q "it delivers each port'"'"'s UDP to the same"'
out=$(run '3\n0\n')
check "iran udp: turned off again" '[ "$(field udp)" = false ] && echo "$out" | grep -q "OFF → turn it on" || [ "$(field udp)" = false ]'
iran false 8443,7778
out=$(run '3\n0\n')
check "iran udp: a user port busy on UDP blocks turning it on" 'echo "$out" | grep -q "UDP port 7778 is already in use" && [ "$(field udp)" = false ]'

# ---- Kharej server ------------------------------------------------------------
kharej 127.0.0.1:8443
out=$(run '1\n2053\n\n0\n')
check "kharej add: Enter = the same port on 127.0.0.1" '[ "$(field port_map)" = 2053 ] && echo "$out" | grep -q "user port 2053 now goes to 127.0.0.1:2053" && echo "$out" | grep -q APPLIED'
check "kharej add: says the port must be open on the Iran server" 'echo "$out" | grep -q "On the Iran server:.* 2053 must be OPEN there"'
out=$(run '1\n2083\n10.0.0.5:443\n0\n')
check "kharej add: a custom target" '[ "$(field port_map)" = "2053,2083=10.0.0.5:443" ]'
out=$(run '1\n2096,2097=127.0.0.1:9\n0\n')
check "kharej add: several at once" '[ "$(field port_map)" = "2053,2083=10.0.0.5:443,2096,2097=127.0.0.1:9" ]'
out=$(run '1\n2098=[::1]:2098\n0\n')
check "kharej add: a bracketed IPv6 target (no glob expansion)" 'echo "$out" | grep -q "user port 2098 now goes to \[::1\]:2098" && echo "$out" | grep -q APPLIED'
out=$(run '2\n2098\n0\n')
out=$(run '1\n2053=bad\n0\n')
check "kharej add: a bad target is refused, file untouched" 'echo "$out" | grep -q "ERR \"2053=bad\": target \"bad\" must be host:port" && [ "$(field port_map)" = "2053,2083=10.0.0.5:443,2096,2097=127.0.0.1:9" ] && ! echo "$out" | grep -q APPLIED'
out=$(run '2\n2083\n0\n')
check "kharej remove: back to the default panel, nothing on the Iran server" '[ "$(field port_map)" = "2053,2096,2097=127.0.0.1:9" ] && echo "$out" | grep -q "now reach the default panel here"'
out=$(run '3\n\n0\n')
check "kharej default: Enter keeps it, no restart" 'echo "$out" | grep -q "INFO Unchanged" && ! echo "$out" | grep -q APPLIED'
out=$(run '3\nnot-an-address\n0\n')
check "kharej default: a bad address is refused" 'echo "$out" | grep -q "Enter host:port" && [ "$(field expose)" = 127.0.0.1:8443 ]'
out=$(run '3\nnone\n0\n')
check "kharej default: none (ports with own targets keep working)" '[ -z "$(field expose)" ] && echo "$out" | grep -q APPLIED'
kharej 127.0.0.1:8443 2053
out=$(run '3\nnone\n2\n2053\n0\n')
check "kharej: removing the last target with no default is refused (mtcp needs one)" 'echo "$out" | grep -q "NOT saved" && [ "$(field port_map)" = 2053 ]'

# dgtun: turning UDP on says what an older Kharej build needs
printf '{"mode":"dial","carrier":"dgtun","encap":"udp","reverse":false,"addr":"1.2.3.4:2096","shared_key":"%s","forward_ports":"8443","udp":false,"iface":"hs9","local_cidr":"10.77.9.1/30","peer_ip":"10.77.9.2","mtu":1280}\n' "$KEY" > "$T/c.json"
out=$(run '3\n0\n')
check "dgtun iran udp: on, and says an older Kharej build needs \"udp\": true" '[ "$(field udp)" = true ] && echo "$out" | grep -q "forwards UDP only"'

# ---- refusals -----------------------------------------------------------------
kharej 127.0.0.1:8443
out=$(run '0\n' "$T/hs2old")
check "older binary: says to upgrade, on both servers" 'echo "$out" | grep -q "predates per-port routing" && echo "$out" | grep -q "on BOTH servers"'
printf '{"mode":"dial","carrier":"udp","addr":"1.2.3.4:2096","shared_key":"%s"}\n' "$KEY" > "$T/c.json"
out=$(run '0\n')
check "IP-tunnel transport: no user ports" 'echo "$out" | grep -q "is an IP tunnel: it has no user ports"'

# ---- setup: ask_port_map --------------------------------------------------------
cat > "$T/apm.sh" <<'APM'
source "$T/core.sh" >/dev/null 2>&1
info(){ echo "INFO $*"; }; warn(){ echo "WARN $*"; }
BIN="$FAKEBIN"; PANEL=127.0.0.1:8443
ask_port_map
echo "PMJ=[$PMJ]"
printf '{"mode":"listen","carrier":"mtcp","addr":"0.0.0.0:2096","shared_key":"%s","cert_file":"%s","key_file":"%s","expose":"%s"%s}\n' \
  "$KEY" "$T/c.pem" "$T/k.pem" "$PANEL" "$PMJ" > "$T/setup.json"
"$T/hs2" check -c "$T/setup.json"
APM
apm(){ printf "$1" | script -qec "T='$T' KEY='$KEY' FAKEBIN='${2:-$T/hs2}' bash '$T/apm.sh'" /dev/null 2>&1 | tr -d '\r'; }
out=$(apm '2053, 2083=127.0.0.1:2096\n')
check "setup: port_map written as entered, and the binary accepts the config" 'echo "$out" | grep -qF "PMJ=[, \"port_map\": \"2053,2083=127.0.0.1:2096\"]" && echo "$out" | grep -q "config OK"'
check "setup: says where every other port goes" 'echo "$out" | grep -q "Every other Iran user port → 127.0.0.1:8443"'
out=$(apm 'abc\n2053,2053\n2053=nohost\n2053\n')
check "setup: re-asks on a bad port, a duplicate and a bad target" '[ "$(echo "$out" | grep -c "try again")" = 3 ] && echo "$out" | grep -qF "PMJ=[, \"port_map\": \"2053\"]"'
out=$(apm '2053=:80\n2053=a:b:80\n2053=[::1]:2096\n')
check "setup: targets the binary would refuse are re-asked at the prompt" '[ "$(echo "$out" | grep -c "try again")" = 2 ] && echo "$out" | grep -qF "PMJ=[, \"port_map\": \"2053=[::1]:2096\"]" && echo "$out" | grep -q "config OK"'
out=$(apm ',,,\n')
check "setup: only commas = none (never an empty port_map)" 'echo "$out" | grep -qF "PMJ=[]"'
out=$(apm '\n')
check "setup: Enter = none (expose only, exactly as before)" 'echo "$out" | grep -qF "PMJ=[]" && echo "$out" | grep -q "config OK"'
out=$(apm '' "$T/hs2old")
check "setup: an older binary is not asked, and says why" 'echo "$out" | grep -qF "PMJ=[]" && echo "$out" | grep -q "Per-port targets need a newer hs2 binary"'
out=$(grep -c '"expose": "\$PANEL"\${PMJ}' "$INST")
check "every kharej template writes the port_map fragment (6)" '[ "$out" = 6 ] && [ "$(grep -c "\"expose\": \"\$PANEL\"" "$INST")" = 6 ]'
out=$(grep -c '^    ask_kharej_targets$' "$INST")
check "every kharej panel question asks the targets too (6)" '[ "$out" = 6 ]'

exit $fail

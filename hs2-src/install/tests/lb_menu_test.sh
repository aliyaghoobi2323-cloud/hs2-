#!/bin/bash
# The Load balancing screen (tunnel menu → l), driven on a pty under the
# installer's real `set -euo pipefail`, with the REAL hs2 binary built from
# this tree doing `hs2 check` (only its "not an IP of this server" finding is
# filtered: the fake `ip` below decides which addresses this server has), and
# fake ip / ss / certbot / restart:
#   - the primary address is always listed and cannot be removed; add, edit,
#     remove, split equally (also 34/33/33);
#   - shares that do not add up to 100 and an address that is not on this
#     server are refused before anything is written;
#   - apply: .prev backup, config written, restart, the lb block to copy to the
#     other server; a failed restart puts .prev back;
#   - on / off and fixed / auto;
#   - a certificate for an address via cert_get_webroot (certbot --webroot),
#     and the offer to turn acme_webroot on when hs2 holds port 80;
#   - live status: the per-path table from lb_paths (header, rows, totals,
#     colours), an older hs2's lb_text decoded, returns on Enter;
#   - a row is picked by its number or its IP;
#   - stage 6, both modes and both roles: apply reloads (SIGHUP) instead of
#     restarting when the running hs2 says lb_reload; the "same change on the
#     other server" note only when the other server does not read
#     announcements; an older hs2 or turning lb off restarts; a reload not
#     taken puts .prev back.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"; SRC=$(cd "$HERE/../.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-900)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP lb_menu_test (no script(1))"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP lb_menu_test (no python3)"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "SKIP lb_menu_test (no go toolchain to build hs2)"; exit 0; }
( cd "$SRC" && CGO_ENABLED=0 go build -o "$T/hs2real" ./cmd/hs2 ) || { echo "FAIL build hs2"; exit 1; }
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$T/k.pem" -out "$T/c.pem" \
  -days 30 -subj /CN=t.example -addext "subjectAltName=DNS:t.example,DNS:t2.example" >/dev/null 2>&1 || { echo "FAIL openssl cert"; exit 1; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
mkdir -p "$T/bin" "$T/le/renewal" "$T/acme"
cat > "$T/bin/hs2" <<FAKE
#!/bin/bash
# hs2 lb history (the live screen's CUTS): a fixed journal summary
if [ "\${1:-} \${2:-}" = "lb history" ]; then cat "$T/cuts.txt" 2>/dev/null; exit 0; fi
if [ "\${1:-}" = check ]; then
  out=\$("$T/hs2real" "\$@" 2>&1); f=\$(printf '%s\n' "\$out" | grep -v "not an IP of this server")
  if printf '%s\n' "\$f" | grep -q '^ERROR'; then printf '%s\n' "\$f"; exit 1; fi
  echo "config OK"; exit 0
fi
exec "$T/hs2real" "\$@"
FAKE
printf '%-40s cuts: %d\n' '9.9.9.1>1.2.3.5' 2 > "$T/cuts.txt"   # the dialing side names the pair
cat > "$T/bin/ip" <<'FAKE'
#!/bin/bash
echo "2: eth0    inet 1.2.3.4/24 brd 1.2.3.255 scope global eth0"
echo "2: eth0    inet 1.2.3.5/24 brd 1.2.3.255 scope global secondary eth0"
echo "2: eth0    inet 1.2.3.6/24 brd 1.2.3.255 scope global secondary eth0"
FAKE
cat > "$T/bin/ss" <<'FAKE'
#!/bin/bash
case "$*" in
  *":80"*) [ -n "${SS80:-}" ] && echo 'LISTEN 0 4096 *:80 *:* users:(("hs2",pid=9,fd=7))' ;;
  *":2096"*) [ -n "${SSBUSY:-}" ] && echo 'LISTEN 0 4096 1.2.3.6:2096 0.0.0.0:* users:(("nginx",pid=5,fd=6))' ;;
esac
exit 0
FAKE
cat > "$T/bin/certbot" <<FAKE
#!/bin/bash
echo "certbot \$*" >> "$T/certbot.log"
FAKE
chmod +x "$T"/bin/*

KEY=d0e7d7f85bdd3190377cf4b97c876f52f3ca7227d95356918b6194442af56e54
iran(){ # extra-json (Iran server, reverse: listens on Iran addresses)
  printf '{"mode":"dial","carrier":"mtcp","reverse":true,"addr":"1.2.3.4:2096","sni":"t.example","shared_key":"%s","forward_ports":"443","cert_file":"%s","key_file":"%s"%s}\n' \
    "$KEY" "$T/c.pem" "$T/k.pem" "${1:-}" > "$T/c.json"
}
cat > "$T/inner.sh" <<'INNER'
export PATH="$T/bin:$PATH"
source "$T/core.sh" >/dev/null 2>&1
say(){ printf '%s\n' "$*"; }; warn(){ say "WARN $*"; }; info(){ say "INFO $*"; }; ok(){ say "OK $*"; }; err(){ say "ERR $*"; }
hr(){ :; }; pause(){ :; }; tm_log_since(){ :; }
ensure_certbot(){ :; }; configure_renewal(){ echo "RENEWAL $1"; }
LE_DIR="$T/le"; CFG_DIR="$T"; ACME_WEBROOT="$T/acme2"
restart_unit(){
  echo "RESTART $1"
  [ -n "${FAILON:-}" ] && grep -q "$FAILON" "$T/c.json" && return 1
  return 0
}
status_path(){ echo "$T/st.json"; }
reload_unit(){ # stage 6: SIGHUP; the fake daemon takes it unless NORELOAD
  echo "RELOAD $1"
  [ -n "${NORELOAD:-}" ] && return 0
  python3 - "$T/st.json" <<'PY'
import json, sys, time
m = json.load(open(sys.argv[1])); m["lb_loaded_at"] = int(time.time()); m["updated"] = int(time.time())
json.dump(m, open(sys.argv[1], "w"))
PY
}
BIN="$T/bin/hs2"
tm_lb hs2-x "$T/c.json"
echo "RETURNED"
INNER
# feed: the keys at once; with '|' a chunk a second, as a person types: the
# live view reads single keys (raw tty), and input sent before the tty goes
# back to line mode is not seen as whole lines by the next menu read.
feed(){ local c cs; case "$1" in *'|'*) IFS='|' read -ra cs <<< "$1"; for c in "${cs[@]}"; do printf "$c"; sleep 1; done ;; *) printf "$1" ;; esac; }
# the status stays fresh (status_fresh) however long the tests take
run(){ [ -f "$T/st.json" ] && sed -i "s/\"updated\": [0-9]*/\"updated\": $(date +%s)/" "$T/st.json"; feed "$1" | script -qec "COLUMNS='${COLUMNS:-120}' LINES='${LINES:-100}' T='$T' FAILON='${FAILON:-}' SS80='${SS80:-}' SSBUSY='${SSBUSY:-}' NORELOAD='${NORELOAD:-}' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
js(){ python3 -c "import json,sys; m=json.load(open('$T/c.json')); print(eval(sys.argv[1]))" "$1"; }

# 1. initial screen: primary listed, Kharej primary unknown in Iran reverse.
iran; out=$(run '0\n')
check "primary listed at 100%, OFF" 'echo "$out" | grep -q " 1) 1.2.3.4 *100% primary" && echo "$out" | grep -q "State: OFF" && echo "$out" | grep -q "(default address)  100%" && echo "$out" | grep -q RETURNED'

# 2. turn on, add 1.2.3.5 with sni (Enter = split), apply.
iran; out=$(run '1\n3\ni\n1.2.3.5\nt2.example\n\n8\n0\n')
check "add + split 50/50" 'echo "$out" | grep -q " 2) 1.2.3.5 *50% · sni t2.example" && echo "$out" | grep -q " 1) 1.2.3.4 *50% primary"'
check "apply: checked, written, restarted" 'echo "$out" | grep -q "config OK" && echo "$out" | grep -q "RESTART hs2-x" && [ "$(js "m[\"lb\"][\"enabled\"]")" = True ] && [ "$(js "[(e[\"ip\"],e[\"weight\"]) for e in m[\"lb\"][\"iran\"]]")" = "[('"'"'1.2.3.4'"'"', 50), ('"'"'1.2.3.5'"'"', 50)]" ]'
check "apply: .prev is the old config (no lb)" '[ -f "$T/c.json.prev" ] && ! grep -q "\"lb\"" "$T/c.json.prev"'
check "apply (reverse, Iran listens): the other server only turns lb ON" 'echo "$out" | grep -q "load balancing only needs to be ON" && echo "$out" | grep -q "1 (turn ON) → 8 (apply)" && echo "$out" | grep -q "No addresses to add there" && ! echo "$out" | grep -q "SAME change on the OTHER server"'
check "apply: block kept for an hs2 too old to read announcements" 'echo "$out" | grep -q "too old to read announcements" && echo "$out" | grep -q "\"lb\": {" && echo "$out" | grep -q "\"sni\": \"t2.example\""'
check "lb is the last key (grep readers keep top-level sni)" '[ "$(jget "$T/c.json" sni 2>/dev/null || grep -o "\"sni\": \"[^\"]*\"" "$T/c.json" | head -1)" != "" ] && [ "$(python3 -c "import json; print(list(json.load(open(\"$T/c.json\")))[-1])")" = lb ]'

# 3. the primary cannot be removed; an extra can (re-split to 100).
out=$(run '4\n1\n4\n2\n\n8\n0\n')
check "primary not removable" 'echo "$out" | grep -q "primary address (addr) — it is always used and cannot be removed"'
check "remove extra, split back to 100" 'echo "$out" | grep -q "Removed 1.2.3.5" && [ "$(js "len(m[\"lb\"][\"iran\"])")" = 1 ] && [ "$(js "m[\"lb\"][\"iran\"][0][\"weight\"]")" = 100 ]'

# 4. split equally with three: 34/33/33.
out=$(run '3\ni\n1.2.3.5\n\n10\n3\ni\n1.2.3.6\n\n10\n6\n8\n0\n')
check "split equally 34/33/33" '[ "$(js "[e[\"weight\"] for e in m[\"lb\"][\"iran\"]]")" = "[34, 33, 33]" ]'

# 5. shares not adding up to 100 are refused, nothing written.
cp "$T/c.json" "$T/before.json"
out=$(run '5\n1\n10\n8\n0\ny\n')
check "sum != 100 refused" 'echo "$out" | grep -q "Iran shares add up to 76%, must be 100%" && echo "$out" | grep -q "Not applied" && cmp -s "$T/c.json" "$T/before.json" && ! echo "$out" | grep -q RESTART'

# 6. an address that is not on this server is refused.
out=$(run '3\ni\n9.9.9.9\n\n\n8\n0\ny\n')
check "missing address warned on add, refused on apply" 'echo "$out" | grep -q "9.9.9.9 is not on this server" && echo "$out" | grep -q "9.9.9.9 is not an address of this server" && cmp -s "$T/c.json" "$T/before.json"'

# 7. a port held by another program on an extra address is refused.
out=$(SSBUSY=1 run '8\n0\n')
check "busy port refused" 'echo "$out" | grep -q "Port 2096 on 1.2.3.6 is already in use" && cmp -s "$T/c.json" "$T/before.json"'

# 8. a failed restart puts the previous config back.
out=$(FAILON='"mode": "auto"' run '2\n8\n0\n')
check "failed restart: .prev restored" 'echo "$out" | grep -q "Restoring the previous config" && echo "$out" | grep -q "Previous config restored" && [ "$(js "m[\"lb\"][\"mode\"]")" = fixed ]'

# 9. auto mode and off.
out=$(run '2\n8\n0\n')
check "mode auto applied" '[ "$(js "m[\"lb\"][\"mode\"]")" = auto ]'
out=$(run '1\n8\n0\n')
check "turned off" '[ "$(js "m[\"lb\"][\"enabled\"]")" = False ] && echo "$out" | grep -q "RESTART hs2-x"'

# 10. certificate via webroot (acme_webroot on): certbot --webroot, paths set,
# and apply refuses while the certificate files are not there.
iran ', "acme_webroot": "'"$T/acme"'", "lb": {"enabled": true, "mode": "fixed", "iran": [{"ip": "1.2.3.4", "weight": 50}, {"ip": "1.2.3.5", "weight": 50, "sni": "t2.example"}], "kharej": []}'
rm -f "$T/certbot.log"
out=$(run '7\n2\n8\n0\ny\n')
check "cert: certbot webroot for the address's sni" 'grep -q -- "certonly --webroot -w $T/acme -d t2.example" "$T/certbot.log" && echo "$out" | grep -q "1.2.3.5 now uses /etc/letsencrypt/live/t2.example/fullchain.pem"'
check "cert: missing files refused on apply" 'echo "$out" | grep -q "1.2.3.5: certificate or key file missing" && echo "$out" | grep -q "Not applied"'
# a valid certificate passes (the binary also checks it covers the sni).
python3 - "$T/c.json" "$T/c.pem" "$T/k.pem" <<'PY'
import json, sys
m = json.load(open(sys.argv[1])); e = m["lb"]["iran"][1]; e["cert_file"], e["key_file"] = sys.argv[2], sys.argv[3]
json.dump(m, open(sys.argv[1], "w"), indent=2)
PY
out=$(run '8\n0\n')
check "cert: valid per-address cert passes" 'echo "$out" | grep -q "config OK" && echo "$out" | grep -q "RESTART hs2-x" && echo "$out" | grep -q "cert 29 days\|cert 30 days"'

# 11. acme_webroot off and hs2 holds port 80: offered, turned on, then certbot.
iran ', "lb": {"enabled": true, "mode": "fixed", "iran": [{"ip": "1.2.3.4", "weight": 50}, {"ip": "1.2.3.5", "weight": 50}], "kharej": []}'
rm -f "$T/certbot.log"
out=$(SS80=1 run '7\n2\nt2.example\ny\n0\ny\n')
check "acme_webroot offered and turned on" 'echo "$out" | grep -q "Port 80 is held by hs2" && [ "$(js "m[\"acme_webroot\"]")" = on ] && echo "$out" | grep -q "RESTART hs2-x" && grep -q -- "--webroot -w $T/acme2 -d t2.example" "$T/certbot.log"'
iran ', "lb": {"enabled": true, "mode": "fixed", "iran": [{"ip": "1.2.3.4", "weight": 50}, {"ip": "1.2.3.5", "weight": 50}], "kharej": []}'
rm -f "$T/certbot.log"
out=$(SS80=1 run '7\n2\nt2.example\nn\n0\ny\n')
check "acme_webroot declined: no certbot, config unchanged" '[ ! -f "$T/certbot.log" ] && ! grep -q acme_webroot "$T/c.json" && echo "$out" | grep -q "No certificate requested"'

# 12. Kharej side of a direct tunnel: Kharej listens; an Iran address belongs to the other server.
printf '{"mode":"listen","carrier":"mtcp","addr":"1.2.3.4:2096","shared_key":"%s","cert_file":"%s","key_file":"%s","expose":"127.0.0.1:443"}\n' "$KEY" "$T/c.pem" "$T/k.pem" > "$T/c.json"
out=$(run '1\n3\ni\n5.5.5.5\n\n0\ny\n')
check "kharej direct: own=Kharej listens, Iran dials, no sni asked for Iran" 'echo "$out" | grep -q "Kharej addresses (this server) — listens" && echo "$out" | grep -q "Iran addresses — dials" && ! echo "$out" | grep -q "Subdomain (sni) for 5.5.5.5" && ! echo "$out" | grep -q "5.5.5.5 is not on this server"'

# 13. live status: lb_text (an older hs2, escaped as Go writes it), Enter returns.
printf '{"updated": %s, "lb_text": "1.2.3.4\\u003eK 50%%/48%% loss 1.0%% q 3ms 4 links"}\n' "$(date +%s)" > "$T/st.json"
out=$(run '9\n\n0\n')
check "live status (older hs2): lb_text decoded + note" 'echo "$out" | grep -q "1.2.3.4>K 50%/48% loss 1.0% q 3ms 4 links" && ! echo "$out" | grep -q "u003e" && echo "$out" | grep -q "update hs2 for the per-path table" && echo "$out" | grep -q RETURNED'
printf '{"updated": %s, "lb_reload": true, "lb_source": "announces"}\n' "$(date +%s)" > "$T/st.json"
out=$(run '9\n\n0\n')
check "live status: lb on, no per-path data: a plain note" 'echo "$out" | grep -q "too old to report each path" && ! echo "$out" | grep -q "not active"'
# the per-path table (lb_paths)
cat > "$T/st.json" <<JS
{"updated": $(date +%s), "role": "Iran side", "dir": "reverse", "lb_side": "listen", "lb_mode": "fixed",
 "lb_reload": true, "lb_source": "announces", "lb_loaded_at": $(( $(date +%s) - 125 )), "lb_peer_advert": true,
 "lb_text": "x\\u003ey",
 "lb_paths": [
  {"local": "1.2.3.4", "remote": "9.9.9.1", "primary": true, "share_pct": 50, "target": 46, "links": 46, "serving": 46, "users": 12,
   "down_mbit": 123.45, "up_mbit": 5.6, "down_mb": 2345.6, "up_mb": 120, "loss_down_pct": 0.4, "loss_up_pct": 0.1,
   "rtt_p50_ms": 42.1, "rtt_p99_ms": 80.2, "min_rtt_ms": 30, "state": "ok"},
  {"local": "1.2.3.5", "remote": "9.9.9.1", "share_pct": 50, "target": 46, "links": 30, "serving": 28, "users": 3,
   "down_mbit": 10, "up_mbit": 1, "down_mb": 100, "up_mb": 10, "loss_down_pct": 7.5,
   "rtt_p50_ms": 50, "rtt_p99_ms": 300, "degraded": 2, "state": "cooling", "cooled_s": 42}],
 "lb_total": {"local": "", "remote": "", "share_pct": 100, "target": 92, "links": 76, "serving": 74, "users": 15,
   "down_mbit": 133.5, "up_mbit": 6.6, "down_mb": 2445.6, "up_mb": 130, "loss_down_pct": 1.2, "loss_up_pct": 0.1,
   "rtt_p50_ms": 45, "rtt_p99_ms": 290, "degraded": 2, "state": "ok"}}
JS
# the new live screen: a 2-line header, one row per Iran IP, the Kharej IPs,
# details on demand (p paths, m matrix, k Kharej IPs, h help), Enter back.
screen(){ printf '%s\n' "$out" | awk '/Load balancing — live/{f=1; n=0; s=""} f{n++; s=s $0 "\n"} /\[Enter\] back/ && f{f=0; last=s; ln=n} END{printf "%s", last; print ln > "/dev/stderr"}' 2>"$T/lines"; }
lines(){ screen >/dev/null; cat "$T/lines"; }
out=$(run '9\n\n0\n')
check "live: header (tunnel, side, mode, peer) in one line" 'echo "$out" | grep -q "hs2-x.* · Iran side, listens · mode fixed · peer .*✓ .*reads announcements" && ! echo "$out" | grep -q "Tunnel:"'
check "live: health line (status word, users, down/up, loss)" 'echo "$out" | grep -q "● watching.*   15 users   ↓ 133.5  ↑ 6.6 Mbit/s   loss 0.9%"'
plain(){ printf '%s\n' "$out" | sed 's/\x1b\[[0-9;]*m//g'; }   # without the colours
pscreen(){ screen | sed 's/\x1b\[[0-9;]*m//g'; }
check "live: one row per Iran IP, by traffic, grouped columns (share | traffic | quality | cuts, state)" 'plain | grep -q "1.2.3.4 *│ .* 50%  *46/46 │  *123.5  *5.6  *12 │  *0.4  *42 │  *0 ok" && plain | grep -q "1.2.3.5 *│ .* 50%  *30/46 │  *10.0  *1.0  *3 │  *7.5  *50 │  *2 cooling 42s, replacing 2" && [ "$(echo "$out" | grep -n "1.2.3.[45] " | head -1 | grep -c 1.2.3.4)" = 1 ] && plain | grep -q "IRAN IP *│ SHARE % *LINKS │ ↓Mbit/s ↑Mbit/s STREAMS │ LOSS % RTT ms │ CUTS STATE"'
R=$(printf '\033[1;31m'); Y=$(printf '\033[1;33m')
check "live: colours only for status (red dot + state of the cooled IP)" 'echo "$out" | grep -qF "${R}●" && echo "$out" | grep -qF "${R}cooling 42s" && ! echo "$out" | grep -q "u003e"'
check "live: Kharej IP line, key hints, no per-path table or legend by default" 'echo "$out" | grep -q "KHAREJ IP" && echo "$out" | grep -q "9.9.9.1 .* 100%" && echo "$out" | grep -q "\[p\] paths  \[m\] matrix  \[k\] Kharej IPs  \[h\] help  \[Enter\] back" && ! echo "$out" | grep -q "THIS SERVER" && ! echo "$out" | grep -q "green ok" && echo "$out" | grep -q RETURNED'
out=$(run '9\n|p|\n|9\n|\n|0\n')
check "live [p]: the full per-path table + totals" 'plain | grep -q "1.2.3.4  *9.9.9.1  *50  *46/46  *61  *12  *123.5  *5.6  *0.4/0.1  *42/80  *0  *2.3G/120M  .*ok, primary" && plain | grep -q "1.2.3.5  *9.9.9.1  *50.*30/46.*39.* 3  *10.0  *1.0.*7.5/-.*50/300  *2 .*cooled 42s, degraded 2" && plain | grep -q "total.*76/92 .* 15  *133.5  *6.6  *1.2/0.1" && echo "$out" | tr -s " " | grep -qF "$R 7.5/-" && echo "$out" | tr -s " " | grep -qF "${Y} 30/46" && plain | grep -q "CUTS = auto cuts in 24 h"'
check "live: the view is kept for the session (paths again on the next visit)" '[ "$(echo "$out" | grep -c "THIS SERVER")" = 2 ]'
out=$(run '9\n|p|p|\n|0\n')
check "live: the same key again goes back to the main table; Enter returns to the menu" '[ "$(echo "$out" | grep -c "THIS SERVER")" = 1 ] && pscreen | grep -q "1.2.3.4 .* 50%  *46/46 │" && [ "$(echo "$out" | grep -c "9) Live view\|Choose")" -ge 2 ] && echo "$out" | grep -q RETURNED'
out=$(run '9\nm\n\n0\n')
check "live [m]: the Iran x Kharej matrix" 'screen | grep -q "LOSS ↓ · AUTO" && screen | grep -q "1.2.3.5 .* 7.5" && ! screen | grep -q "USERS"'
out=$(run '9\nk\n\n0\n')
check "live [k]: the full Kharej-IP table" 'pscreen | grep -q "KHAREJ IP *│ SHARE % *LINKS" && pscreen | grep -q "9.9.9.1 *│ .*100%  *76/92 │  *133.4  *6.6  *15 │  *0.9  *45 │  *2 ok"'
out=$(run '9\nh\n\n0\n')
check "live [h]: legend + addresses / last load / direction" 'screen | grep -q "green ok" && screen | grep -q "Addresses: from this config (announced to the other server)" && screen | grep -q "(2m ago)" && screen | grep -q "tunnel reverse" && pscreen | grep -q "1.2.3.4 .* 50%  *46/46 │" && screen | grep -q "MTU = path MTU median"'
out=$(LANG=C LC_ALL= LC_CTYPE= run '9\n\n0\n')
check "live: a non-UTF-8 locale gets ASCII (* dots, # bars)" 'screen | grep -q "\*.* 1.2.3.4 .*#" && ! screen | grep -q "●\|█\|·"'
out=$(COLUMNS=80 run '9\n\n0\n')
check "live: under 100 columns RTT, CUTS, UP and LINKS are left out" 'pscreen | grep -q "IRAN IP .*STREAMS .*LOSS" && ! pscreen | grep -q "RTT\|↑Mbit\|CUTS\|LINKS" && pscreen | grep -q "↓Mbit"'

# a production-like status on both servers: 6 IPv4 Iran IPs (3 cut at ~13%
# loss with no links, 3 at ~3.5%), 1 IPv6 at ~2% carrying most users; Kharej
# IPs 198.51.100.113, 203.0.113.67, 2001:db8:1::1.
prodst(){ # iran|kharej
  python3 - "$1" "$T/st.json" <<'PY'
import json, sys, time
side, out = sys.argv[1], sys.argv[2]
now = int(time.time())
K4 = ["198.51.100.113", "203.0.113.67"]
K6 = "2001:db8:1::1"
V6 = "2001:db8:5:2:1d4:3e:91c0:94d2"
# iran ip, loss per kharej ip, eff, users, down, up, rtt, cut
IR = [
    ("203.0.113.41", (13.1, 13.8), 0.7, 0, 0, 0, 0, True),
    ("203.0.113.10", (12.2, 12.9), 0.7, 0, 0, 0, 0, True),
    ("203.0.113.20", (13.6, 14.2), 0.7, 0, 0, 0, 0, True),
    ("203.0.113.71", (3.2, 3.8), 24.0, 31, 9.4, 1.1, 61, False),
    ("203.0.113.72", (3.4, 3.7), 23.5, 28, 8.1, 0.9, 64, False),
    ("203.0.113.73", (3.3, 3.9), 23.4, 24, 6.7, 0.8, 66, False),
    (V6, (2.0,), 27.0, 329, 61.3, 5.2, 58, False),
]
VERD = ["Iran IP %s lossy on every kharej IP" % ip for ip, *_r, cut in IR if cut]
pairs, rpaths = [], []
for ip, ls, eff, users, dn, up, rtt, cut in IR:
    ks = [K6] if ":" in ip else K4
    for n, k in enumerate(ks):
        f = 1.0 / len(ks)
        sp = 100.0 / 7 * f
        links = 0 if cut else int(round(eff * 2.4 * f))
        p = {"local": k if side == "kharej" else ip, "remote": ip if side == "kharej" else k,
             "share_pct": round(sp, 1), "eff_pct": round(eff * f, 1), "target": links, "links": links, "serving": links,
             "users": int(users * f) + (users % 2 if n == 0 and len(ks) > 1 else 0) if len(ks) > 1 else users,
             "down_mbit": round(dn * f, 2), "up_mbit": round(up * f, 2),
             "down_mb": round(dn * f * 3600 * 7, 1), "up_mb": round(up * f * 3600 * 7, 1),
             "loss_down_pct": ls[n], "loss_up_pct": 0.3 if not cut else None,
             "rtt_p50_ms": rtt + 3 * n if rtt else None, "rtt_p99_ms": rtt * 3 if rtt else None,
             "state": "ok", "auto_factor": 0.05 if cut else (0.96 if ls[n] < 3 else 0.88),
             "auto_reason": ("cut: Iran IP %s lossy on every kharej IP" % ip) if cut else "ok",
             "new_pct": round(eff * f, 1)}
        if side == "iran":
            p["loss_down_src"] = "sender"; p["loss_down_rx_pct"] = round(ls[n] * 0.8, 1)
        for kk in [k for k, v in p.items() if v is None]: del p[kk]
        if ip == "203.0.113.41" and n == 0: p["primary"] = True
        pairs.append(p)
        rpaths.append({"local": k, "remote": ip, "eff_pct": p["eff_pct"], "target": links, "links": links,
                       "factor": p["auto_factor"], "loss_pct": ls[n], "raw_pct": ls[n], "cut": cut,
                       "reason": p["auto_reason"] if cut else ""})
def total(rows):
    t = {"local": "", "remote": "", "share_pct": 100, "state": "ok"}
    for f in ("target", "links", "serving", "users"):
        t[f] = sum(r.get(f, 0) for r in rows)
    for f in ("down_mbit", "up_mbit", "down_mb", "up_mb"):
        t[f] = round(sum(r.get(f, 0) for r in rows), 1)
    w = sum(r["down_mbit"] for r in rows)
    t["loss_down_pct"] = round(sum(r["loss_down_pct"] * r["down_mbit"] for r in rows) / w, 1)
    t["loss_up_pct"] = 0.3; t["rtt_p50_ms"] = 60; t["rtt_p99_ms"] = 190
    return t
st = {"updated": now, "lb_mode": "auto", "lb_auto2": True, "lb_reload": True, "lb_peer_advert": True,
      "lb_peer_at": now - 2, "lb_loaded_at": now - 7 * 3600, "lb_verdicts": VERD, "users": 412, "mbit": 85.5}
if side == "kharej":
    st.update({"role": "Kharej side", "dir": "reverse", "lb_side": "dial", "lb_source": "advert", "lb_paths": pairs})
else:
    # listening side: lb_paths merges the dialer's IPs per Iran address; lb_pairs per pair
    rows = []
    for ip, ls, eff, users, dn, up, rtt, cut in IR:
        ps = [p for p in pairs if p["local"] == ip]
        r = {"local": ip, "remote": ",".join(p["remote"] for p in ps), "share_pct": round(sum(p["share_pct"] for p in ps), 1),
             "eff_pct": eff, "state": "ok", "auto_factor": ps[0]["auto_factor"], "auto_reason": ps[0]["auto_reason"],
             "new_pct": eff, "loss_down_src": "sender"}
        for f in ("target", "links", "serving", "users"): r[f] = sum(p[f] for p in ps)
        for f in ("down_mbit", "up_mbit", "down_mb", "up_mb"): r[f] = round(sum(p[f] for p in ps), 2)
        r["loss_down_pct"] = round(sum(p["loss_down_pct"] for p in ps) / len(ps), 1)
        if not cut: r["loss_up_pct"] = 0.3; r["rtt_p50_ms"] = rtt; r["rtt_p99_ms"] = rtt * 3
        if ip == "203.0.113.41": r["primary"] = True
        rows.append(r)
    st.update({"role": "Iran side", "dir": "reverse", "lb_side": "listen", "lb_source": "announces",
               "lb_mode_from": "peer", "lb_paths": rows, "lb_pairs": pairs,
               "lb_report": {"auto": True, "auto2": True, "paths": rpaths, "verdicts": VERD,
                             "at": time.strftime("%Y-%m-%dT%H:%M:%S.000000000Z", time.gmtime(now - 2))}})
st["lb_total"] = total(pairs)
json.dump(st, open(out, "w"), indent=1)
PY
}
for s in iran kharej; do
  prodst "$s"; out=$(COLUMNS=100 run '9\n\n0\n')
  check "production ($s): the main view fits ~20 lines" '[ "$(lines)" -le 20 ]'
  check "production ($s): the IPv6 IP first (most traffic), truncated" 'pscreen | grep -A1 "IRAN IP" | grep -q "●.* 2001:db8…:94d2 .* 27%  *65/65 │  *61.3  *329 │  *2.0 │ ok"'
  check "production ($s): the 3 cut IPs on one dim line" 'screen | grep -q "3 cut (no traffic): 203.0.113.41 · 203.0.113.10 · 203.0.113.20 — loss 13–14%" && [ "$(screen | grep -c 203.0.113.41)" = 1 ]'
  check "production ($s): 3 Kharej IPs, one verdict line, status watching" 'screen | grep -q "2001:db8:1::1" && screen | grep -q "198.51.100.113 .* 36%" && screen | grep -q "203.0.113.67 .* 36%" && [ "$(screen | grep -c "lossy on every")" = 1 ] && screen | grep -q "3 Iran IPs lossy on every Kharej IP (13–14%)" && screen | grep -q "● watching" && ! screen | grep -q "Verdict:"'
done
# lbauto3: a report older than 15 s (or the edge's "report stale") is shown as
# stale and its figures are not used; a Kharej-IP verdict reads as one line.
prodst iran
python3 - "$T/st.json" <<'PY'
import json, sys, time
m = json.load(open(sys.argv[1]))
m["lb_report"]["at"] = time.strftime("%Y-%m-%dT%H:%M:%S.000000000Z", time.gmtime(time.time() - 40))
for p in m["lb_pairs"] + m["lb_paths"]:
    if p.get("auto_reason") == "ok": p["auto_reason"] = "report stale"; p["auto_factor"] = 1
m["lb_verdicts"] = ["Kharej IP 203.0.113.67 lossy on every Iran IP"]; m["lb_report"]["verdicts"] = []
json.dump(m, open(sys.argv[1], "w"))
PY
out=$(COLUMNS=100 run '9\n\n0\n')
check "lbauto3: a stale report in the header, status watching" 'screen | grep -q "report stale (4[0-9]s ago)" && screen | grep -q "● watching"'
check "lbauto3: a Kharej-IP verdict in one line" 'screen | grep -q "→ Kharej IP 203.0.113.67 lossy on every Iran IP.*: the IP, not the paths" && [ "$(screen | grep -c "lossy on every")" = 1 ]'
# [p] on the exit (Kharej) side: no up-loss reading on any path -> "LOSS D"
# only, and the legend names only the columns shown (no NEW when hidden).
prodst kharej
python3 - "$T/st.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
for p in m["lb_paths"]:
    p.pop("loss_up_pct", None); p.pop("new_pct", None)
json.dump(m, open(sys.argv[1], "w"))
PY
out=$(COLUMNS=100 run '9\np\n\n0\n')
check "[p] Kharej side: LOSS D only, legend without hidden columns" 'screen | grep -q "TOTAL D/U" && screen | grep -q " LOSS D  " && ! screen | grep -q "LOSS D/U\|/-\|NEW = " && screen | grep -q "LOSS D = data resent by the sender" && pscreen | grep -q " 13.1 "'
# redraw in place: every line ends in erase-to-end-of-line, the frame in
# erase-below; the whole screen is cleared only when the view changes.
K=$(printf '\033[K'); J=$(printf '\033[J'); CLR=$(printf '\033[2J')
prodst iran
out=$(COLUMNS=100 run '9\n|||||\n|0\n')
check "live redraw: lines end in ESC[K, frames in ESC[J, one full clear without a view change" '[ "$(echo "$out" | grep -c "Load balancing — live")" -ge 2 ] && [ "$(echo "$out" | grep -cF "$CLR")" = 1 ] && screen | grep -F "IRAN IP" | grep -qF "$K" && echo "$out" | grep -qF "$J"'
out=$(COLUMNS=100 run '9\n|p|p|\n|0\n')
check "live redraw: a full clear on each view change" '[ "$(echo "$out" | grep -cF "$CLR")" = 3 ]'
# a frame taller than the terminal is cut to fit, the key hints kept
out=$(COLUMNS=100 LINES=12 run '9\n\n0\n')
check "live: a frame taller than the terminal is cut (… N more, hints kept)" '[ "$(lines)" -le 11 ] && screen | grep -q "… [0-9]* more lines — \[p\] for all paths" && screen | grep -q "\[p\] paths  \[m\] matrix"'

# b304: MTU (median, the lowest link when lower, low MTU flagged), cuts in
# 24 h (`hs2 lb history`), parked IPs, all columns on a wide terminal.
prodst iran
python3 - "$T/st.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
for p in m["lb_pairs"] + m["lb_paths"]:
    l = p.get("local", "")
    if "2001:db8:5:" in l: p["pmtu_down"] = {"pmtu_min": 1076, "pmtu_med": 1500, "pmtu_usual": 1500, "links": 5}; p["pmtu_up"] = {"pmtu_min": 1500, "pmtu_med": 1500, "pmtu_usual": 1500, "links": 5}
    elif l == "203.0.113.71": p["pmtu_down"] = {"pmtu_min": 1200, "pmtu_med": 1200, "pmtu_usual": 1500, "links": 5, "pmtu_drops": 1, "last_drop_s": 20}
    else: p["pmtu_down"] = {"pmtu_min": 1500, "pmtu_med": 1500, "pmtu_usual": 1500, "links": 5}
    if l == "203.0.113.73": p["loss_up_pct"] = 11.2   # an upload problem: the LOSS column shows it, marked ↑
json.dump(m, open(sys.argv[1], "w"))
PY
printf '[{"unit": "hs2-x", "ip": "203.0.113.72", "until": %s, "prior": []}]\n' $(( $(date +%s) + 7200 )) > "$T/lb-park-hs2-x.json"
printf '%-40s cuts: %d\n' '198.51.100.113>203.0.113.73' 3 '203.0.113.67>203.0.113.73' 1 > "$T/cuts.txt"
out=$(COLUMNS=200 HS2_PARK_DIR="$T" run '9\n\n0\n')
check "MTU: median, '(min N)' when a link is lower, low MTU flagged (main view)" 'pscreen | grep -q "IRAN IP .*│ LOSS % RTT ms MTU  *│ CUTS STATE" && pscreen | grep -q "2001:db8…:94d2 .* 1500 (min 1076) │  *0 ok" && pscreen | grep -q "203.0.113.71 .* 1200  *│  *0 lowered, low MTU 1200" && screen | grep -qF "${R}1200 "'
check "cuts in 24 h per IP, either pair order (main view)" 'pscreen | grep -q "203.0.113.73 .*│  *4 ↑ loss 11%, lowered" && pscreen | grep -q "198.51.100.113 .*│  *3 .*lowered"'
check "parked IPs on one line" 'pscreen | grep -q "parked: 203.0.113.72 until [0-9][0-9]:[0-9][0-9]"'
check "LOSS: the worse of ↓ and ↑, ↑ marked (1 % or more) and red over 5 %, named in the state" 'pscreen | grep -q "203.0.113.73 .*│  *11.2↑ " && screen | grep -qF "${R} 11.2↑" && pscreen | grep -q "203.0.113.71 .*│  *3.5  " && ! pscreen | grep -q "203.0.113.72 .*↑ "'
out=$(COLUMNS=230 HS2_PARK_DIR="$T" run '9\np\n\n0\n')
check "[p]: MTU D/U with min and a recent drop, CUTS, set/eff share" 'pscreen | grep -q "MTU D/U .*CUTS" && pscreen | grep -q "2001:db8…:94d2 .*1500/1500 (min 1076)  *0 " && pscreen | grep -q "203.0.113.71 .*1200/- drop 20s ago  *0 " && pscreen | grep -q "203.0.113.73 .* 4  *[0-9.]*[MGT]/" && pscreen | grep -q "set/eff %"'
rm -f "$T/lb-park-hs2-x.json"
# hs2-menu lb [tunnel] --once: one frame from the shell (a bare name finds
# hs2-NAME); install_self also writes the hs2-menu-lb wrapper.
mkdir -p "$T/units"; printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c %s\n' "$T/c.json" > "$T/units/hs2-x.service"
cli(){ sed -i "s/\"updated\": [0-9]*/\"updated\": $(date +%s)/" "$T/st.json"
  bash -c 'set -euo pipefail; TD=$1; source "$TD/core.sh" >/dev/null 2>&1; set +e; HS2_DIED=1; CFG_DIR=$TD; UNIT_DIR=$TD/units; BIN=$TD/bin/hs2
  status_path(){ echo "$TD/st.json"; }; shift; lb_live_cli "$@"' _ "$T" "$@" 2>&1; }
out=$(LANG=C.UTF-8 COLUMNS=140 cli x --once); rc=$?
check "hs2-menu lb x --once: one frame, no terminal needed" '[ $rc = 0 ] && pscreen | grep -q "hs2-x · Iran side, listens" && pscreen | grep -q "IRAN IP .*STREAMS" && ! echo "$out" | grep -qF "$CLR"'
out=$(LANG=C.UTF-8 cli --once --view paths); rc=$?
check "hs2-menu lb --once: the only lb tunnel is picked; --view paths" '[ $rc = 0 ] && echo "$out" | grep -q "THIS SERVER"'
out=$(cli nosuch --once); rc=$?
check "hs2-menu lb: an unknown tunnel or view is refused" '[ $rc != 0 ] && echo "$out" | grep -q "No tunnel .hs2-nosuch\|No tunnel .nosuch" && ! cli x --once --view bogus >/dev/null'
bash -c 'source "$1/core.sh" >/dev/null 2>&1; MENU_BIN=$1/menu; MENU_LB_BIN=$1/menu-lb; install_self' "$INST" "$T" >/dev/null 2>&1
check "install_self: hs2-menu-lb is a wrapper that execs hs2-menu lb" '[ -x "$T/menu-lb" ] && grep -qxF "exec $T/menu lb \"\$@\"" "$T/menu-lb" && cmp -s "$T/menu" "$INST"'

# 14. not offered for a transport without lb.
printf '{"mode":"dial","carrier":"tls","addr":"1.2.3.4:2096"}\n' > "$T/c.json"
out=$(run '\n')
check "tls: refused with the reason" 'echo "$out" | grep -q "mtcp / l3mtcp / l3) only"'

# 15. stage 6. st PEER: a running stage-6 hs2 (lb_reload) whose peer does / does not read announcements.
st(){ printf '{"updated": %s, "lb_reload": true, "lb_loaded_at": 1, "lb_peer_advert": %s}
' "$(date +%s)" "$1" > "$T/st.json"; }
LBON='"lb": {"enabled": true, "mode": "fixed", "iran": [{"ip": "1.2.3.4", "weight": 50}, {"ip": "1.2.3.5", "weight": 50}], "kharej": []}'
# reverse, Iran listens: reload, no restart, nothing to repeat on the other server
iran ", $LBON"; st true
out=$(run '2
8
0
')
check "reverse iran (listens): reload, no restart" 'echo "$out" | grep -q "RELOAD hs2-x" && ! echo "$out" | grep -q "RESTART hs2-x" && echo "$out" | grep -q "Applied without a restart" && echo "$out" | grep -q "nothing to repeat there" && ! echo "$out" | grep -q "Make the SAME change" && [ "$(js "m[\"lb\"][\"mode\"]")" = auto ]'
# the other server is older (no announcements): the copy note stays
iran ", $LBON"; st false
out=$(run '2
8
0
')
check "reverse iran, peer not known: reload + turn-ON note, not same-change" 'echo "$out" | grep -q "RELOAD hs2-x" && echo "$out" | grep -q "load balancing only needs to be ON" && ! echo "$out" | grep -q "Make the SAME change"'
# a row picked by its IP as well as by its number
iran ", $LBON"; st true
out=$(run '5
1.2.3.5
70

0
y
')
check "lb_pick: the IP picks the row" 'echo "$out" | grep -q "Share for 1.2.3.5 in % \[50\]" && echo "$out" | grep -q "Shares now total 120%" && ! echo "$out" | grep -q "Invalid choice"'
out=$(run '5
7.7.7.7
0
')
check "lb_pick: an IP not in the list" 'echo "$out" | grep -q "7.7.7.7 is not in the list"'
# direct, Kharej listens
KLB='"lb": {"enabled": true, "mode": "fixed", "kharej": [{"ip": "1.2.3.4", "weight": 50}, {"ip": "1.2.3.5", "weight": 50}], "iran": []}'
printf '{"mode":"listen","carrier":"mtcp","addr":"1.2.3.4:2096","shared_key":"%s","cert_file":"%s","key_file":"%s","expose":"127.0.0.1:443",%s}
' "$KEY" "$T/c.pem" "$T/k.pem" "$KLB" > "$T/c.json"; st true
out=$(run '2
8
0
')
check "direct kharej (listens): roles shown, reload, no restart" 'echo "$out" | grep -q "Kharej addresses (this server) — listens" && echo "$out" | grep -q "RELOAD hs2-x" && ! echo "$out" | grep -q "RESTART hs2-x" && echo "$out" | grep -q "nothing to repeat there"'
# direct, Iran dials (its source addresses)
printf '{"mode":"dial","carrier":"mtcp","addr":"9.9.9.1:2096","bind_local_ip":"1.2.3.4","sni":"t.example","shared_key":"%s","forward_ports":"443","lb":{"enabled":true,"mode":"fixed","iran":[{"ip":"1.2.3.4","weight":50},{"ip":"1.2.3.5","weight":50}],"kharej":[{"ip":"9.9.9.1","weight":100}]}}
' "$KEY" > "$T/c.json"; st true
out=$(run '2
8
0
')
check "direct iran (dials): roles shown, reload" 'echo "$out" | grep -q "Iran addresses (this server) — dials" && echo "$out" | grep -q "RELOAD hs2-x" && ! echo "$out" | grep -q "RESTART hs2-x"'
# reverse, Kharej dials
printf '{"mode":"listen","reverse":true,"carrier":"mtcp","addr":"9.9.9.1:2096","bind_local_ip":"1.2.3.4","sni":"t.example","shared_key":"%s","expose":"127.0.0.1:443","lb":{"enabled":true,"mode":"fixed","kharej":[{"ip":"1.2.3.4","weight":50},{"ip":"1.2.3.5","weight":50}],"iran":[{"ip":"9.9.9.1","weight":100}]}}
' "$KEY" > "$T/c.json"; st true
out=$(run '2
8
0
')
check "reverse kharej (dials): roles shown, reload" 'echo "$out" | grep -q "Kharej addresses (this server) — dials" && echo "$out" | grep -q "RELOAD hs2-x" && ! echo "$out" | grep -q "RESTART hs2-x"'
# an older hs2 (no lb_reload in its status): restart as before
iran ", $LBON"; printf '{"updated": %s}
' "$(date +%s)" > "$T/st.json"
out=$(run '2
8
0
')
check "older hs2: restart" 'echo "$out" | grep -q "RESTART hs2-x" && ! echo "$out" | grep -q "RELOAD hs2-x"'
# turning lb off needs the restart
iran ", $LBON"; st true
out=$(run '1
8
0
')
check "lb off: restart, not reload" 'echo "$out" | grep -q "RESTART hs2-x" && ! echo "$out" | grep -q "RELOAD hs2-x"'
# the daemon does not take the reload: .prev back
iran ", $LBON"; st true
out=$(NORELOAD=1 run '2
8
0
')
check "reload not taken: .prev restored" 'echo "$out" | grep -q "did not take the change" && [ "$(js "m[\"lb\"][\"mode\"]")" = fixed ]'
exit "$fail"

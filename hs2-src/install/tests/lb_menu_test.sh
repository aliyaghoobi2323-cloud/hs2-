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
#   - live status shows lb_text and returns on Enter;
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
if [ "\${1:-}" = check ]; then
  out=\$("$T/hs2real" "\$@" 2>&1); f=\$(printf '%s\n' "\$out" | grep -v "not an IP of this server")
  if printf '%s\n' "\$f" | grep -q '^ERROR'; then printf '%s\n' "\$f"; exit 1; fi
  echo "config OK"; exit 0
fi
exec "$T/hs2real" "\$@"
FAKE
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
run(){ printf "$1" | script -qec "T='$T' FAILON='${FAILON:-}' SS80='${SS80:-}' SSBUSY='${SSBUSY:-}' NORELOAD='${NORELOAD:-}' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
js(){ python3 -c "import json,sys; m=json.load(open('$T/c.json')); print(eval(sys.argv[1]))" "$1"; }

# 1. initial screen: primary listed, Kharej primary unknown in Iran reverse.
iran; out=$(run '0\n')
check "primary listed at 100%, OFF" 'echo "$out" | grep -q " 1) 1.2.3.4 *100% primary" && echo "$out" | grep -q "State: OFF" && echo "$out" | grep -q "(default address)  100%" && echo "$out" | grep -q RETURNED'

# 2. turn on, add 1.2.3.5 with sni (Enter = split), apply.
iran; out=$(run '1\n3\ni\n1.2.3.5\nt2.example\n\n8\n0\n')
check "add + split 50/50" 'echo "$out" | grep -q " 2) 1.2.3.5 *50% · sni t2.example" && echo "$out" | grep -q " 1) 1.2.3.4 *50% primary"'
check "apply: checked, written, restarted" 'echo "$out" | grep -q "config OK" && echo "$out" | grep -q "RESTART hs2-x" && [ "$(js "m[\"lb\"][\"enabled\"]")" = True ] && [ "$(js "[(e[\"ip\"],e[\"weight\"]) for e in m[\"lb\"][\"iran\"]]")" = "[('"'"'1.2.3.4'"'"', 50), ('"'"'1.2.3.5'"'"', 50)]" ]'
check "apply: .prev is the old config (no lb)" '[ -f "$T/c.json.prev" ] && ! grep -q "\"lb\"" "$T/c.json.prev"'
check "apply: reminder + block for the other server" 'echo "$out" | grep -q "SAME change on the OTHER server" && echo "$out" | grep -q "\"lb\": {" && echo "$out" | grep -q "\"sni\": \"t2.example\""'
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

# 13. live status: lb_text, Enter returns.
printf '{"updated": %s, "lb_text": "1.2.3.4>K 50%%/48%% loss 1.0%% q 3ms 4 links"}\n' "$(date +%s)" > "$T/st.json"
out=$(run '9\n\n0\n')
check "live status shows lb_text" 'echo "$out" | grep -q "1.2.3.4>K 50%/48% loss 1.0% q 3ms 4 links" && echo "$out" | grep -q RETURNED'

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
check "reverse iran, older peer: reload + same-change note" 'echo "$out" | grep -q "RELOAD hs2-x" && echo "$out" | grep -q "Make the SAME change"'
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

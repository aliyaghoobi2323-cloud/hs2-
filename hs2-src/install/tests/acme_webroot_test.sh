#!/bin/bash
# lb stage 3: certificates through hs2's own ACME HTTP-01 responder.
#   - cfg_acme_webroot reads "acme_webroot" like hs2 does (off/on/path).
#   - cert_get_webroot runs certbot --webroot -w <webroot> when it is on (a stub
#     records the exact command; no real certificate is ever requested), and
#     falls back to standalone when it is off.
#   - cert_renewal_to_webroot switches a standalone renewal config to webroot
#     (backup kept, standalone keys dropped, a webroot_map per name) and leaves
#     every other authenticator alone; cert_offer_webroot_switch changes only
#     what the user confirms.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "${out:-}" | tr '\n' ' ' | cut -c1-300)"; fail=1; fi; }
command -v openssl >/dev/null 2>&1 || { echo "SKIP acme_webroot_test (needs openssl)"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
LE="$T/le"; mkdir -p "$LE/live" "$LE/renewal" "$T/bin"

# certbot stub: records its arguments, never talks to Let's Encrypt.
cat > "$T/bin/certbot" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >> "$CERTBOT_LOG"
exit "${CERTBOT_RC:-0}"
STUB
chmod +x "$T/bin/certbot"
export CERTBOT_LOG="$T/certbot.log"

mkcert(){ # DIR NAMES...
  local d="$1"; shift; local san=""; for n in "$@"; do san="$san${san:+,}DNS:$n"; done
  mkdir -p "$d"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 80 \
    -subj "/CN=$1" -addext "subjectAltName=$san" \
    -keyout "$d/privkey.pem" -out "$d/fullchain.pem" >/dev/null 2>&1
  cp "$d/fullchain.pem" "$d/cert.pem"
}

run(){ # shell code, with the installer's functions loaded
  PATH="$T/bin:$PATH" bash -c 'source "$1/core.sh" >/dev/null 2>&1; LE_DIR="$2"; ACME_WEBROOT="$1/acme"
    port_free(){ return 0; }; configure_renewal(){ :; }; systemctl(){ :; }
    eval "$3"' _ "$T" "$LE" "$1" 2>&1
}

printf '{"mode":"dial","forward_ports":"80,443"}\n' > "$T/off.json"
printf '{"mode":"dial","forward_ports":"80,443","acme_webroot": "on"}\n' > "$T/on.json"
printf '{"mode":"dial","acme_webroot":"/srv/acme/"}\n' > "$T/path.json"
printf '{"mode":"dial","acme_webroot":"off"}\n' > "$T/offx.json"

out=$(run 'echo "[$(cfg_acme_webroot '"$T/off.json"')][$(cfg_acme_webroot '"$T/offx.json"')]"')
check "acme_webroot absent / off -> empty" '[ "$out" = "[][]" ]'
out=$(run 'cfg_acme_webroot '"$T/on.json"'')
check "acme_webroot on -> the installer webroot" '[ "$out" = "$T/acme" ]'
out=$(run 'cfg_acme_webroot '"$T/path.json"'')
check "acme_webroot path -> that path (no trailing /)" '[ "$out" = "/srv/acme" ]'

: > "$CERTBOT_LOG"
out=$(run 'cert_get_webroot '"$T/on.json"' t2.example.com')
check "webroot: certbot certonly --webroot -w <webroot> -d <sub>" 'grep -qx "certonly --webroot -w $T/acme -d t2.example.com --non-interactive --agree-tos --register-unsafely-without-email --deploy-hook pkill -HUP -x hs2" "$CERTBOT_LOG"'
check "webroot: challenge directory created" '[ -d "$T/acme/.well-known/acme-challenge" ]'
check "webroot: prints the cert|key paths" 'echo "$out" | grep -q "^/etc/letsencrypt/live/t2.example.com/fullchain.pem|/etc/letsencrypt/live/t2.example.com/privkey.pem"'
check "webroot: never standalone" '! grep -q standalone "$CERTBOT_LOG"'

: > "$CERTBOT_LOG"
out=$(run 'cert_get_webroot '"$T/off.json"' t3.example.com')
check "off: falls back to certbot --standalone" 'grep -q "^certonly --standalone -d t3.example.com" "$CERTBOT_LOG"'

: > "$CERTBOT_LOG"
out=$(CERTBOT_RC=1 run 'cert_get_webroot '"$T/on.json"' t4.example.com; echo "rc=$?"')
check "webroot failure: dies with the reason" 'echo "$out" | grep -q "webroot) failed for t4.example.com" && ! echo "$out" | grep -q "rc=0"'

# renewal switch
mkcert "$LE/live/sa" sa.example.com t2.example.com '*.example.com'
printf 'renew_before_expiry = 30 days\nversion = 2.9.0\n[renewalparams]\nauthenticator = standalone\nhttp01_port = 80\nserver = https://acme-v02.api.letsencrypt.org/directory\nrenew_hook = pkill -HUP -x hs2\n' > "$LE/renewal/sa.conf"
mkcert "$LE/live/dns" dns.example.com
printf '[renewalparams]\nauthenticator = manual\n' > "$LE/renewal/dns.conf"
cp "$LE/renewal/dns.conf" "$T/dns.orig"
cp "$LE/renewal/sa.conf" "$T/sa.orig"

out=$(printf 'n\n' | run 'cert_offer_webroot_switch '"$T/on.json"'')
check "offer, answered no: standalone config unchanged" 'cmp -s "$LE/renewal/sa.conf" "$T/sa.orig" && echo "$out" | grep -q "left as standalone"'
out=$(printf 'y\n' | run 'cert_offer_webroot_switch '"$T/off.json"'')
check "offer with acme_webroot off: nothing changes" 'cmp -s "$LE/renewal/sa.conf" "$T/sa.orig"'

out=$(printf 'y\n' | run 'cert_offer_webroot_switch '"$T/on.json"'')
C="$LE/renewal/sa.conf"
check "offer, yes: authenticator = webroot" 'grep -qx "authenticator = webroot" "$C" && ! grep -q standalone "$C"'
check "webroot_path set, http01_port dropped" 'grep -qx "webroot_path = $T/acme," "$C" && ! grep -q http01_port "$C"'
check "webroot_map has every non-wildcard name" 'grep -qx "\[\[webroot_map\]\]" "$C" && grep -qx "sa.example.com = $T/acme" "$C" && grep -qx "t2.example.com = $T/acme" "$C" && ! grep -q "^\*" "$C"'
check "other keys kept (renew_before_expiry on top, hook, server)" '[ "$(head -n1 "$C")" = "renew_before_expiry = 30 days" ] && grep -q "^renew_hook = pkill -HUP -x hs2" "$C" && grep -q "^server = " "$C"'
check "backup of the standalone config" 'cmp -s "$C.hs2-standalone" "$T/sa.orig"'
check "the manual (DNS-01) config untouched, never asked" 'cmp -s "$LE/renewal/dns.conf" "$T/dns.orig" && ! echo "$out" | grep -q "^dns renews"'
cp "$C" "$T/sa.after"
out=$(run 'if cert_renewal_to_webroot '"$C"' '"$T/acme"'; then echo rc=0; else echo rc=1; fi')
check "switch is idempotent (a webroot config is not touched again)" 'echo "$out" | grep -q "rc=1" && cmp -s "$C" "$T/sa.after"'
out=$(run 'cert_conf_get '"$C"' authenticator')
check "installer reads it back as webroot" '[ "$out" = webroot ]'

[ $fail = 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }

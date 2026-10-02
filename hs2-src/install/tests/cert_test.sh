#!/bin/bash
# Certificate status & renewal in the tunnel manager, against REAL certificates
# (openssl; an expired one via a tiny Go helper) in a fake certbot tree:
#   - tm_cert_line tells the truth for every renewal method: DNS-01 (--manual,
#     no hook) never renews by itself; standalone needs port 80; overdue; own
#     certificate; no renewal config; expired; nothing on the dialing side.
#   - cert_domains keeps a wildcard name literal (no glob expansion).
#   - configure_renewal no longer claims "renews automatically" for DNS-01.
#   - the c) Certificate action, driven through the REAL tm_tunnel_menu on a
#     pty under the installer's real `set -euo pipefail`, runs exactly the right
#     certbot command (a stub records it) — or none, when port 80 is taken.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "${out:-}" | tr '\n' ' ' | cut -c1-300)"; fail=1; fi; }
command -v openssl >/dev/null 2>&1 || { echo "SKIP cert_test (needs openssl)"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
LE="$T/le"; mkdir -p "$LE/live" "$LE/renewal" "$T/own"

# mkcert DIR DAYS: a real self-signed cert valid for DAYS, covering a normal and
# a wildcard name.
mkcert(){
  mkdir -p "$1"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days "$2" \
    -subj "/CN=vpn.example.com" -addext "subjectAltName=DNS:vpn.example.com,DNS:*.example.com" \
    -keyout "$1/privkey.pem" -out "$1/fullchain.pem" >/dev/null 2>&1
}
# lineage NAME DAYS CONF: a certbot lineage with a renewal config (CONF may be "-" for none)
lineage(){ mkcert "$LE/live/$1" "$2"; [ "$3" = - ] || printf '%b' "$3" > "$LE/renewal/$1.conf"; }
cfgfor(){ # name certfile mode reverse -> a config path
  printf '{"mode":"%s","reverse":%s,"carrier":"mtcp","cert_file":"%s","key_file":"%s"}\n' \
    "$3" "$4" "$2" "${2%/*}/privkey.pem" > "$T/$1.json"; echo "$T/$1.json"
}
line(){ # cfg [port80busy] -> tm_cert_line output
  bash -c 'source "$1/core.sh" >/dev/null 2>&1; LE_DIR="$2"; C_Y=""; C_R=""; C_0=""
           if [ "$4" = busy ]; then port_free(){ return 1; }; else port_free(){ return 0; }; fi
           tm_cert_line "$3"' _ "$T" "$LE" "$1" "${2:-free}" 2>&1
}

lineage dns80   80 '[renewalparams]\nauthenticator = manual\n'
lineage dns20   20 '[renewalparams]\nauthenticator = manual\n'
lineage hook80  80 '[renewalparams]\nauthenticator = manual\nmanual_auth_hook = /x.sh\n'
lineage sa80    80 '[renewalparams]\nauthenticator = standalone\n'
lineage sa20    20 '[renewalparams]\nauthenticator = standalone\n'
lineage sa20rb  20 'renew_before_expiry = 10 days\n[renewalparams]\nauthenticator = standalone\n'
lineage off80   80 '[renewalparams]\nauthenticator = standalone\nautorenew = False\n'
lineage noconf  80 -
lineage saph    80 '[renewalparams]\nauthenticator = standalone\npre_hook = systemctl stop nginx\n'
lineage sa8888  80 '[renewalparams]\nauthenticator = standalone\nhttp01_port = 8888\n'
mkcert "$T/own/far" 200; mkcert "$T/own/near" 10

out=$(line "$(cfgfor a "$LE/live/dns80/fullchain.pem" listen false)")
check "DNS-01, far from expiry: says it is renewed by hand" 'echo "$out" | grep -q "DNS-01: renew it by hand before"'
out=$(line "$(cfgfor a "$LE/live/dns20/fullchain.pem" listen false)")
check "DNS-01, 20 days left: says it does NOT renew by itself" 'echo "$out" | grep -q "DNS-01 does NOT renew by itself"'
out=$(line "$(cfgfor a "$LE/live/hook80/fullchain.pem" listen false)")
check "manual WITH an auth hook is automatic" 'echo "$out" | grep -q "renews automatically"'
out=$(line "$(cfgfor a "$LE/live/sa80/fullchain.pem" listen false)")
check "standalone, port 80 free: renews automatically" 'echo "$out" | grep -q "renews automatically"'
out=$(line "$(cfgfor a "$LE/live/sa80/fullchain.pem" listen false)" busy)
check "standalone, port 80 taken: warns the next renewal will fail" 'echo "$out" | grep -q "port 80 is in use"'
out=$(line "$(cfgfor a "$LE/live/saph/fullchain.pem" listen false)" busy)
check "standalone, port 80 taken but a pre_hook frees it: no false alarm" 'echo "$out" | grep -q "renews automatically"'
out=$(line "$(cfgfor a "$LE/live/sa8888/fullchain.pem" listen false)" busy)
check "standalone on http01_port 8888, taken: names port 8888" 'echo "$out" | grep -q "port 8888 is in use"'
out=$(line "$(cfgfor a "$LE/live/sa20/fullchain.pem" listen false)")
check "standalone, 20 days left (renews at 30): overdue" 'echo "$out" | grep -q "overdue"'
out=$(line "$(cfgfor a "$LE/live/sa20rb/fullchain.pem" listen false)")
check "renew_before_expiry = 10 days is respected (20 left is fine)" 'echo "$out" | grep -q "renews automatically"'
out=$(line "$(cfgfor a "$LE/live/off80/fullchain.pem" listen false)")
check "autorenew = False: renewal NOT set up" 'echo "$out" | grep -q "NOT set up"'
out=$(line "$(cfgfor a "$LE/live/noconf/fullchain.pem" listen false)")
check "lineage without a renewal config: renewal NOT set up" 'echo "$out" | grep -q "NOT set up"'
out=$(line "$(cfgfor a "$T/own/far/fullchain.pem" listen false)")
check "own certificate, far: you renew it" 'echo "$out" | grep -q "your own certificate (you renew it)"'
out=$(line "$(cfgfor a "$T/own/near/fullchain.pem" listen false)")
check "own certificate, 10 days: replace its files" 'echo "$out" | grep -q "replace its files before"'
out=$(line "$(cfgfor a "$LE/live/dns20/fullchain.pem" dial false)")
check "direct DIAL side holds no certificate: no line" '[ -z "$out" ]'
out=$(line "$(cfgfor a "$LE/live/dns20/fullchain.pem" dial true)")
check "reverse edge (dial+reverse) LISTENS: line shown" 'echo "$out" | grep -q "DNS-01"'

if command -v go >/dev/null 2>&1; then
  mkdir -p "$T/gen" "$LE/live/expired"
  cat > "$T/gen/main.go" <<'GO'
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"time"
)

func main() {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "vpn.example.com"},
		DNSNames: []string{"vpn.example.com"}, NotBefore: time.Now().Add(-100 * 24 * time.Hour), NotAfter: time.Now().Add(-48 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	pem.Encode(os.Stdout, &pem.Block{Type: "CERTIFICATE", Bytes: der})
}
GO
  if (cd "$T/gen" && GOFLAGS= GO111MODULE=off go run main.go) > "$LE/live/expired/fullchain.pem" 2>/dev/null; then
    printf '[renewalparams]\nauthenticator = manual\n' > "$LE/renewal/expired.conf"
    out=$(line "$(cfgfor a "$LE/live/expired/fullchain.pem" listen false)")
    check "expired: says EXPIRED and that the tunnel still works" 'echo "$out" | grep -q "EXPIRED on" && echo "$out" | grep -q "tunnel still works"'
  else echo "SKIP expired-cert case (go run failed)"; fi
else echo "SKIP expired-cert case (no go)"; fi

# cert_domains: the wildcard name must stay literal even where it could glob.
mkdir -p "$T/globdir"; touch "$T/globdir/a.example.com" "$T/globdir/b.example.com"
out=$(cd "$T/globdir" && bash -c 'source "$1/core.sh" >/dev/null 2>&1; cert_domains "$2"' _ "$T" "$LE/live/sa80/fullchain.pem")
check "cert_domains keeps *.example.com literal (no glob expansion)" '[ "$out" = "vpn.example.com *.example.com" ]'

# configure_renewal's closing message per method.
cr(){ bash -c 'source "$1/core.sh" >/dev/null 2>&1; LE_DIR="$2"; systemctl(){ :; }; configure_renewal "$3"' _ "$T" "$LE" "$1" 2>&1; }
out=$(cr dns80)
check "configure_renewal, DNS-01: warns it does NOT renew by itself" 'echo "$out" | grep -q "does NOT renew by itself" && ! echo "$out" | grep -q "Renewal set"'
out=$(cr sa80)
check "configure_renewal, standalone: Renewal set" 'echo "$out" | grep -q "Renewal set"'
check "configure_renewal kept the hot-reload hook + 30-day window" 'grep -q "^renew_hook = pkill -HUP -x hs2" "$LE/renewal/sa80.conf" && head -1 "$LE/renewal/sa80.conf" | grep -q "^renew_before_expiry = 30 days"'

# ---- the c) Certificate action, through the real menu on a pty --------------
if ! command -v script >/dev/null 2>&1; then echo "SKIP pty tests (no script(1))"; exit "$fail"; fi
mkdir -p "$T/bin"
cat > "$T/bin/certbot" <<'SH'
#!/bin/bash
printf '%s\n' "$*" >> "$CERTBOT_LOG"
exit 0
SH
chmod +x "$T/bin/certbot"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh" >/dev/null 2>&1
LE_DIR="$T/le"; PATH="$T/bin:$PATH"
tm_cfg(){ echo "$CFGF"; }; tm_details(){ :; }; tm_autostart(){ return 0; }
say(){ printf '%s\n' "$*"; }; info(){ say "$*"; }; ok(){ say "$*"; }; warn(){ say "$*"; }; err(){ say "$*"; }; hr(){ :; }
if [ "$P80" = busy ]; then port_free(){ return 1; }; else port_free(){ return 0; }; fi
outer(){ tm_tunnel_menu hs2-x; echo "BACK_IN_CALLER"; }
outer
INNER
act(){ # cfg port80 keys -> menu output; certbot calls land in $T/certbot.log
  : > "$T/certbot.log"
  printf "$3" | script -qec "T='$T' CFGF='$1' P80=$2 CERTBOT_LOG='$T/certbot.log' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
calls(){ cat "$T/certbot.log"; }

out=$(act "$(cfgfor m "$LE/live/dns20/fullchain.pem" listen false)" free 'c\n1\n\n0\n')
check "menu shows c) Certificate on the certificate side" 'echo "$out" | grep -q "c) Certificate"'
check "DNS-01 renew: certbot certonly --manual dns on the SAME lineage with every name" \
  'calls | grep -qx -- "certonly --manual --preferred-challenges dns --cert-name dns20 -d vpn.example.com -d \*.example.com --force-renewal --agree-tos --register-unsafely-without-email --deploy-hook pkill -HUP -x hs2"'
check "DNS-01 renew: menu returns afterwards (no set -e crash)" 'echo "$out" | grep -q BACK_IN_CALLER'
out=$(act "$(cfgfor m "$LE/live/sa80/fullchain.pem" listen false)" free 'c\n1\n\n0\n')
check "standalone, test: certbot renew --dry-run on the lineage" 'calls | grep -qx -- "renew --cert-name sa80 --dry-run" && echo "$out" | grep -q "automatic renewal works"'
out=$(act "$(cfgfor m "$LE/live/sa80/fullchain.pem" listen false)" free 'c\n2\n\n0\n')
check "standalone, renew now: certbot renew --force-renewal" 'calls | grep -qx -- "renew --cert-name sa80 --force-renewal" && echo "$out" | grep -q "Renewed"'
out=$(act "$(cfgfor m "$LE/live/sa80/fullchain.pem" listen false)" busy 'c\n1\n\n0\n')
check "standalone, port 80 taken: certbot is NOT run, operator told why" '[ ! -s "$T/certbot.log" ] && echo "$out" | grep -q "Port 80 is in use"'
out=$(act "$(cfgfor m "$LE/live/saph/fullchain.pem" listen false)" busy 'c\n1\n\n0\n')
check "port 80 taken but a pre_hook frees it: the dry run DOES run" 'calls | grep -qx -- "renew --cert-name saph --dry-run"'
out=$(act "$(cfgfor m "$LE/live/hook80/fullchain.pem" listen false)" free 'c\n1\n\n0\n')
check "manual WITH a hook: offers the automatic test (dry run), not a by-hand renewal" 'calls | grep -qx -- "renew --cert-name hook80 --dry-run"'
out=$(act "$(cfgfor m "$T/own/far/fullchain.pem" listen false)" free 'c\n\n0\n')
check "own certificate: explains how to replace it, runs no certbot" '[ ! -s "$T/certbot.log" ] && echo "$out" | grep -q "replace these two files"'
out=$(act "$(cfgfor m "$LE/live/sa80/fullchain.pem" dial false)" free 'c\n0\n')
check "dial side: no c) entry, c is just an invalid choice" '! echo "$out" | grep -q "c) Certificate" && [ ! -s "$T/certbot.log" ] && echo "$out" | grep -q BACK_IN_CALLER'

exit "$fail"

#!/usr/bin/env bash
# ============================================================================
#  hs2 — DPI-resistant tunnel (stream mode over multi-link TLS)
#  Runs ALONGSIDE Backhaul without touching it.
#      bash install.sh            (menu)
#      bash install.sh upgrade    (update an existing install in place)
# ============================================================================
set -euo pipefail

BIN=/usr/local/bin/hs2
REPO_RAW="${HS2_REPO_RAW:-https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main}"
CFG=/etc/hs2/config.json
SVC=/etc/systemd/system/hs2.service
TUN_SUBNET_IRAN="10.77.0.1/30"
TUN_SUBNET_KHAREJ="10.77.0.2/30"
TUN_PEER_IRAN="10.77.0.2"
TUN_PEER_KHAREJ="10.77.0.1"
# Link pool written into new Iran configs (tuned in the lab; see BUILD.md).
LINK_MIN=8
LINK_MAX=16
LINK_PER=8

# ---------- pretty output (all to stderr so $(...) captures only real values) --
_c(){ printf '\033[%sm%s\033[0m\n' "$1" "$2" >&2; }
info(){ _c '1;34' "→ $1"; }
ok(){   _c '1;32' "✓ $1"; }
warn(){ _c '1;33' "! $1"; }
err(){  _c '1;31' "✗ $1"; }
die(){  err "$1"; exit 1; }
hr(){   _c '0;36' "────────────────────────────────────────────"; }

[ "$(id -u)" = 0 ] || die "Please run as root."

# ---------- helpers ----------------------------------------------------------
port_free(){ ! ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }
udp_port_free(){ ! ss -Hlun "sport = :$1" 2>/dev/null | grep -q .; }

# transport_to_carrier maps the transport choice to the binary's carrier value.
# tcp keeps the chosen TLS mode; udp and auto are the datagram carriers.
transport_to_carrier(){
  case "$1" in
    udp)  echo udp ;;
    auto) echo auto ;;
    *)    echo "${2:-mtcp}" ;;   # tcp: use the TLS mode
  esac
}

show_ips(){ ip -4 -br addr 2>/dev/null | awk '$1!="lo"{print $3}' | sed 's#/.*##' | sed 's/^/   /' >&2; }

first_public_ip(){ ip -4 -br addr 2>/dev/null | awk '$1!="lo"{print $3}' | sed 's#/.*##' | head -1; }

install_prereqs(){
  info "Installing prerequisites…"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q >/dev/null 2>&1 || true
  apt-get install -y -q iproute2 iptables curl ca-certificates >/dev/null 2>&1 || true
  # tun module
  modprobe tun 2>/dev/null || true
  tune_kernel
  ok "Prerequisites ready."
}

# BBR copes with lossy long-haul links far better than cubic, fq paces it, and
# a low notsent_lowat keeps the kernel from queueing seconds of data on each
# tunnel link (the delay users see under load).
tune_kernel(){
  modprobe tcp_bbr 2>/dev/null || true
  cat > /etc/sysctl.d/99-hs2.conf <<'EOF'
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.ipv4.tcp_notsent_lowat = 131072
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.tcp_mtu_probing = 1
EOF
  if sysctl -p /etc/sysctl.d/99-hs2.conf >/dev/null 2>&1; then
    ok "Kernel network tuning applied (BBR, fq, low send-queue latency)."
  else
    warn "Some kernel tuning could not be applied (see /etc/sysctl.d/99-hs2.conf)."
  fi
}

install_binary(){
  local d tmp; tmp=$(mktemp)
  d=$(cd "$(dirname "$0")" 2>/dev/null && pwd || pwd)
  # Download the latest from GitHub first, so a stale binary lying around can
  # never be reinstalled by mistake. Only if GitHub is unreachable (Iran
  # server) fall back to a hs2-linux-amd64 next to install.sh or in the
  # current directory. ALWAYS replace the old binary.
  info "Downloading hs2 binary from GitHub…"
  if curl -fL --connect-timeout 10 --retry 2 -o "$tmp" "$REPO_RAW/hs2-linux-amd64" 2>/dev/null; then
    ok "Downloaded."
  elif [ -f "$d/hs2-linux-amd64" ] || [ -f "./hs2-linux-amd64" ]; then
    local f="$d/hs2-linux-amd64"; [ -f "$f" ] || f="./hs2-linux-amd64"
    warn "GitHub unreachable — using local $f (make sure it is the NEW one)."
    cp "$f" "$tmp"
  else
    rm -f "$tmp"
    die "download failed. On the Iran server, copy hs2-linux-amd64 from the kharej server into $(pwd) and run again."
  fi
  chmod 755 "$tmp"
  "$tmp" version 2>/dev/null | grep -q "hs2 v3" \
    || { rm -f "$tmp"; die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."; }
  systemctl stop hs2 2>/dev/null || true
  install -m755 "$tmp" "$BIN"; rm -f "$tmp"
  info "sha256: $(sha256sum "$BIN" | cut -c1-16)…"
  "$BIN" version 2>/dev/null | grep -q "hs2 v3"     || die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."
  ok "Installed $("$BIN" version 2>/dev/null)"
}

write_service(){
  local role="$1"
  cat > "$SVC" <<EOF
[Unit]
Description=hs2 DPI-resistant tunnel ($role)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN run -c $CFG
Restart=always
RestartSec=3
TimeoutStopSec=8
KillMode=mixed
KillSignal=SIGTERM
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

start_service(){
  local role="$1"
  systemctl enable --now hs2 >/dev/null 2>&1
  sleep 2
  if systemctl is-active --quiet hs2; then
    ok "hs2 ($role) is running."
  else
    err "hs2 failed to start. Last log:"
    journalctl -u hs2 -n 20 --no-pager >&2
    exit 1
  fi
}

encode_link(){ printf '%s' "$1" | base64 -w0; }
decode_link(){ printf '%s' "$1" | base64 -d 2>/dev/null; }

# ---------- certificate ------------------------------------------------------
# The certificate always lives on whichever side is the TLS SERVER:
#   direct : kharej is the TLS server -> cert on kharej
#   reverse: iran   is the TLS server -> cert on iran
# get_cert is therefore called from kharej_listener (direct) and from
# iran_listener (reverse), always on the machine that terminates TLS.

# resolve_a prints the A records a domain resolves to, one per line (best-effort
# across the tools that might be present).
resolve_a(){
  local d="$1"
  if command -v getent >/dev/null 2>&1; then
    getent ahostsv4 "$d" 2>/dev/null | awk '{print $1}' | sort -u
  elif command -v dig >/dev/null 2>&1; then
    dig +short A "$d" 2>/dev/null
  elif command -v host >/dev/null 2>&1; then
    host -t A "$d" 2>/dev/null | awk '/has address/{print $NF}'
  elif command -v nslookup >/dev/null 2>&1; then
    nslookup -type=A "$d" 2>/dev/null | awk '/^Address: /{print $2}'
  fi
}

# check_domain_ip warns (loudly, but does not abort) when the domain does not
# resolve to the IP of the server that will terminate TLS. Getting this wrong is
# the #1 reason HTTP-01 validation fails, so the user is told before certbot runs.
check_domain_ip(){ # domain expected_ip
  local d="$1" ip="$2" got
  [ -n "$d" ] && [ "$d" != "-" ] || return 0
  # `|| true`: an NXDOMAIN must warn, not abort the script under `set -e`.
  got=$(resolve_a "$d" || true)
  if [ -z "$got" ]; then
    warn "$d does not resolve to any IP yet."
    warn "For HTTP-01 (port 80) validation its A record must point to THIS server ($ip)."
    return 0
  fi
  if printf '%s\n' "$got" | grep -qx "$ip"; then
    ok "$d resolves to $ip (this server) — good."
  else
    warn "$d resolves to: $(printf '%s ' $got)"
    warn "…which is NOT this server's IP ($ip)."
    warn "HTTP-01 (port 80) validation will fail unless the A record points here."
    warn "Fix the A record, or use DNS-01 / an existing certificate below."
  fi
}

ensure_certbot(){
  command -v certbot >/dev/null 2>&1 && return 0
  info "Installing certbot…"
  apt-get install -y -q certbot >/dev/null 2>&1 \
    || die "could not install certbot. Install it manually, or re-run and choose 'existing certificate'."
}

# cert_standalone: Let's Encrypt HTTP-01 on port 80 (the classic path).
cert_standalone(){ # domain
  local domain="$1"
  ensure_certbot
  port_free 80 || die "port 80 is busy — free it, or re-run and choose DNS-01 / an existing certificate."
  info "Getting Let's Encrypt certificate (standalone HTTP-01 on port 80)…"
  if certbot certonly --standalone -d "$domain" --non-interactive --agree-tos \
       --register-unsafely-without-email --deploy-hook "systemctl restart hs2" >/dev/null 2>&1; then
    ok "Certificate obtained for $domain."
  else
    err "certbot HTTP-01 failed for $domain."
    warn "Usual causes on an Iran server: inbound port 80 is filtered, the A record"
    warn "does not point here, or a firewall blocks it."
    warn "Re-run this setup and choose DNS-01 (no port 80) or 'existing certificate'."
    die  "certificate not obtained."
  fi
  printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
}

# cert_dns01: Let's Encrypt DNS-01. Needs no inbound port 80 — ideal when the
# Iran server's inbound 80 is filtered. certbot pauses and prints a TXT record
# for the operator to add, so this prompt is interactive (reads from the tty).
cert_dns01(){ # domain
  local domain="$1"
  ensure_certbot
  echo >&2
  info "DNS-01: certbot will print a _acme-challenge TXT record for $domain."
  info "Add it at your DNS provider, wait ~1 min for it to propagate, then continue in certbot."
  if certbot certonly --manual --preferred-challenges dns -d "$domain" --agree-tos \
       --register-unsafely-without-email --deploy-hook "systemctl restart hs2" </dev/tty >&2; then
    ok "Certificate obtained for $domain via DNS-01."
  else
    err "certbot DNS-01 did not complete for $domain."
    warn "Obtain a certificate another way and re-run choosing 'existing certificate'."
    die  "certificate not obtained."
  fi
  printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
}

# cert_existing: the user already has a cert/key pair (bought, wildcard, or from
# another tool). We validate the files rather than fail silently at runtime.
cert_existing(){ # domain
  local domain="$1" c k
  read -rp "Path to certificate (fullchain) PEM: " c </dev/tty
  read -rp "Path to private key PEM: " k </dev/tty
  [ -n "$c" ] && [ -f "$c" ] || die "certificate file not found: '$c'"
  [ -n "$k" ] && [ -f "$k" ] || die "key file not found: '$k'"
  if command -v openssl >/dev/null 2>&1; then
    openssl x509 -in "$c" -noout >/dev/null 2>&1 || die "'$c' is not a valid PEM certificate."
    openssl pkey -in "$k" -noout >/dev/null 2>&1 || warn "'$k' does not parse as a PEM key — double-check it."
    # Best-effort: confirm the cert actually covers the domain the peer expects.
    # openssl -checkhost always exits 0, so match on its output text instead.
    if [ -n "$domain" ] && [ "$domain" != "-" ]; then
      if openssl x509 -in "$c" -noout -checkhost "$domain" 2>/dev/null | grep -q "does match"; then
        ok "Certificate covers $domain."
      else
        warn "Certificate does not appear to cover $domain — the peer's SNI must match its names, or TLS auth will fail."
      fi
    fi
  fi
  ok "Using existing certificate: $c"
  printf '%s|%s' "$c" "$k"
}

# get_cert obtains a cert for THIS (TLS-server) side and prints "cert|key".
# It first checks DNS, reuses an existing Let's Encrypt cert if present, and
# otherwise offers HTTP-01 / DNS-01 / bring-your-own — so a filtered port 80 on
# the Iran side never means a silent failure.
get_cert(){ # domain expected_ip
  local domain="$1" expip="${2:-}"
  check_domain_ip "$domain" "$expip"
  if [ -f "/etc/letsencrypt/live/$domain/fullchain.pem" ]; then
    info "Reusing existing Let's Encrypt certificate for $domain"
    printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
    return 0
  fi
  echo >&2
  echo "  Certificate for $domain (this server terminates TLS):" >&2
  echo "    1) Let's Encrypt — HTTP-01, standalone on port 80 (needs port 80 open + A record here)" >&2
  echo "    2) Let's Encrypt — DNS-01, add a TXT record (no port 80 — best when inbound 80 is filtered)" >&2
  echo "    3) I already have a certificate (give the file paths)" >&2
  read -rp "Choose [1]: " CM </dev/tty
  case "${CM:-1}" in
    1) cert_standalone "$domain" ;;
    2) cert_dns01 "$domain" ;;
    3) cert_existing "$domain" ;;
    *) die "invalid certificate choice" ;;
  esac
}

# ---------- KHAREJ (foreign server) ------------------------------------------
# ask_transport prints the transport menu and sets the global TRANSPORT.
ask_transport(){
  echo >&2
  echo "  Transport:" >&2
  echo "    1) auto — UDP+FEC when the path allows it, silent TCP fallback (recommended)" >&2
  echo "    2) udp  — UDP+FEC only (best on high, bursty packet loss; needs UDP open)" >&2
  echo "    3) tcp  — TLS multi-link only (the original transport)" >&2
  echo "    4) tun  — Backhaul-style L3 IP tunnel over multi-link TLS (mtcp): a routed" >&2
  echo "              interface, real TLS encryption, high throughput (no user ports)" >&2
  read -rp "Choose [1]: " TR </dev/tty
  case "${TR:-1}" in
    1) TRANSPORT=auto ;;
    2) TRANSPORT=udp ;;
    3) TRANSPORT=tcp ;;
    4) TRANSPORT=tun ;;
    *) die "invalid transport" ;;
  esac
}

# ask_tun_params sets TUNIF (interface name, cosmetic/per-server) and TUNMTU for
# tun mode. The MTU is carried in the hs2:// link so both sides always match; the
# interface name may differ per server.
ask_tun_params(){
  read -rp "TUN interface name on THIS server [hs0]: " TUNIF </dev/tty; TUNIF=${TUNIF:-hs0}
  case "$TUNIF" in ''|*[!a-zA-Z0-9_-]*) die "invalid interface name" ;; esac
}
ask_tun_mtu(){
  read -rp "TUN MTU (1320 matches Backhaul; kept in sync with the other side) [1320]: " TUNMTU </dev/tty
  TUNMTU=${TUNMTU:-1320}
  case "$TUNMTU" in ''|*[!0-9]*) die "MTU must be a number" ;; esac
}

# ask_direction sets DIRECTION=direct|reverse. Direction is WHO STARTS the
# connection; it does not change which side has the users (iran) or the panel
# (kharej). In direct, Iran dials out to Kharej (classic). In reverse, Kharej
# dials in to Iran, so the first SYN originates abroad — better when
# outbound-from-Iran is filtered, or Kharej is behind NAT.
ask_direction(){
  echo >&2
  echo "  Direction (who starts the tunnel connection):" >&2
  echo "    1) direct  — Iran dials out to Kharej (default, classic)" >&2
  echo "    2) reverse — Kharej dials in to Iran (SYN starts abroad)" >&2
  read -rp "Choose [1]: " DIR </dev/tty
  case "${DIR:-1}" in 1) DIRECTION=direct ;; 2) DIRECTION=reverse ;; *) die "invalid direction" ;; esac
}

# The LISTENER side always generates the link (it knows its own endpoint); the
# DIALER side pastes it. Direction decides which physical server is which:
#   direct : listener = kharej (exit) , dialer = iran (edge)
#   reverse: listener = iran  (edge) , dialer = kharej (exit)

show_link(){ # endpoint domain shared panel carrier udp transport direction [mtu]
  local L; L=$(encode_link "$1|$2|$3|$4|$5|$6|$7|$8|${9:-}")
  echo >&2; hr
  ok "SETUP LINK — copy it to the OTHER server:"
  _c '1;33' "hs2://$L"
  hr
}

# parse_link reads a pasted hs2:// link into ENDPOINT DOMAIN SHARED PANEL
# CARRIER UDP TRANSPORT DIRECTION MTU (with sensible defaults for older links;
# the trailing MTU field is optional and only used by tun mode).
parse_link(){
  read -rp "Paste the hs2:// setup link from the OTHER server: " RAW </dev/tty
  RAW=${RAW#hs2://}
  local DEC; DEC=$(decode_link "$RAW") || die "invalid link"
  IFS='|' read -r ENDPOINT DOMAIN SHARED PANEL CARRIER UDP TRANSPORT DIRECTION MTU <<< "$DEC"
  [ -n "$ENDPOINT" ] && [ -n "$SHARED" ] || die "link is missing fields"
  CARRIER=${CARRIER:-mtcp}; UDP=${UDP:-false}
  if [ -z "$TRANSPORT" ]; then
    case "$CARRIER" in udp) TRANSPORT=udp ;; auto) TRANSPORT=auto ;; *) TRANSPORT=tcp ;; esac
  fi
  DIRECTION=${DIRECTION:-direct}
}

# ---------- KHAREJ (foreign server, the panel side) --------------------------
setup_kharej(){
  hr; info "KHAREJ setup (foreign server — the panel side)"; hr
  install_prereqs
  install_binary
  ask_direction
  if [ "$DIRECTION" = "direct" ]; then
    echo >&2; info "This server's IP addresses:"; show_ips
    local defip; defip=$(first_public_ip)
    read -rp "Public IP of THIS kharej server [$defip]: " PUBIP </dev/tty; PUBIP=${PUBIP:-$defip}
    [ -n "$PUBIP" ] || die "public IP required"
    ask_transport
    kharej_listener      # direct: kharej listens and generates the link
  else
    kharej_dialer        # reverse: kharej dials the iran edge (pastes the link)
  fi
}

# kharej_listener: direct exit. Kharej listens for the iran edge and generates
# the link. (This is the classic flow.)
kharej_listener(){
  read -rp "Tunnel port (clients never see this) [2096]: " TPORT </dev/tty; TPORT=${TPORT:-2096}
  local SHARED; SHARED=$(openssl rand -hex 32)
  local PANEL="-" LMTU=""
  mkdir -p "$(dirname "$CFG")"

  if [ "$TRANSPORT" = "tcp" ]; then
    read -rp "Panel inbound address on this server [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    read -rp "Domain (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required"
    echo >&2
    echo "  TLS mode:  1) mtcp (recommended)  2) l3mtcp  3) tls" >&2
    read -rp "Choose [1]: " M </dev/tty
    case "${M:-1}" in 1) CARRIER=mtcp ;; 2) CARRIER=l3mtcp ;; 3) CARRIER=tls ;; *) die "invalid mode" ;; esac
    read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
    case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "0.0.0.0:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "expose": "$PANEL"
}
EOF
  elif [ "$TRANSPORT" = "tun" ]; then
    # Backhaul-style L3 tunnel over multi-link TLS (l3mtcp). Kharej is the TLS
    # server here, so the cert lives on kharej (like direct tcp). No user ports:
    # it is a routed interface, not a port forwarder.
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    read -rp "Domain (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required"
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; CARRIER=l3mtcp; UDP=false
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "l3mtcp", "reverse": false,
  "addr": "0.0.0.0:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": $TUNMTU,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY"
}
EOF
  else
    udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    [ "$TRANSPORT" = "auto" ] && { port_free "$TPORT" || die "auto also needs TCP port $TPORT free — pick another."; }
    CARRIER=$(transport_to_carrier "$TRANSPORT"); DOMAIN="-"; UDP=false
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "0.0.0.0:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED"
}
EOF
  fi
  chmod 600 "$CFG"; write_service kharej; start_service kharej
  ok "KHAREJ ready (direct, transport: $TRANSPORT)."
  show_link "$PUBIP:$TPORT" "$DOMAIN" "$SHARED" "$PANEL" "$CARRIER" "$UDP" "$TRANSPORT" "direct" "$LMTU"
  if [ "$TRANSPORT" = "tun" ]; then
    info "L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (MTU $LMTU)."
    info "Your panel stays on this kharej; iran reaches it over the tunnel."
  fi
  info "On the Iran server: bash install.sh → 2 (Iran) → direction 'direct' → paste the link."
}

# kharej_dialer: reverse exit. Kharej DIALS the iran edge; it pastes the link
# iran generated (which carries iran's endpoint) and forwards to the panel.
kharej_dialer(){
  parse_link
  [ "$DIRECTION" = "reverse" ] || die "this link is a DIRECT link; for reverse, generate the link on the IRAN side first."
  read -rp "Dial out FROM which local IP? (Enter = automatic): " EGRESSIP </dev/tty
  ok "Link OK — will dial the iran edge at $ENDPOINT (transport $TRANSPORT)."
  mkdir -p "$(dirname "$CFG")"
  if [ "$TRANSPORT" = "tcp" ]; then
    read -rp "Panel inbound address on this server [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": true,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
  "shared_key": "$SHARED",
  "expose": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    ok "KHAREJ ready (reverse, tcp). It dials in to the Iran edge and forwards to $PANEL."
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse tun: kharej DIALS the iran edge (TLS client) and runs the L3 pipe.
    # MTU comes from the link so both sides match.
    [ -n "$MTU" ] || die "this link has no MTU field — regenerate it on the iran edge with the new installer."
    ask_tun_params
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "l3mtcp", "reverse": true,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": $MTU,
  "shared_key": "$SHARED",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    ok "KHAREJ ready (reverse, tun / L3 over multi-link TLS). It dials in to the Iran edge."
    info "L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (MTU $MTU)."
    info "Your panel stays on this kharej; iran reaches it over the tunnel."
  else
    # udp/auto: TUN IP tunnel on hs0, no panel forwarding here.
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": true,
  "addr": "$ENDPOINT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED", "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    ok "KHAREJ ready (reverse, $TRANSPORT / UDP+FEC). It dials in to the Iran edge."
    info "An IP tunnel is up on hs0 (kharej 10.77.0.2, iran 10.77.0.1). Route panel traffic over hs0."
  fi
  info "Backhaul is untouched. Status/logs any time:  bash install.sh → 4"
}

# ---------- IRAN (the user-facing edge) --------------------------------------
setup_iran(){
  hr; info "IRAN setup (the edge — where users connect)"; hr
  install_prereqs
  install_binary
  ask_direction
  if [ "$DIRECTION" = "direct" ]; then
    iran_dialer          # direct: iran dials out to kharej (pastes the link)
  else
    echo >&2; info "This server's IP addresses:"; show_ips
    local defip; defip=$(first_public_ip)
    read -rp "Public IP of THIS iran server [$defip]: " PUBIP </dev/tty; PUBIP=${PUBIP:-$defip}
    [ -n "$PUBIP" ] || die "public IP required"
    ask_transport
    iran_listener        # reverse: iran listens for kharej and generates the link
  fi
}

# iran_dialer: direct edge. Iran dials out to kharej; it pastes kharej's link.
iran_dialer(){
  parse_link
  [ "$DIRECTION" = "direct" ] || die "this link is a REVERSE link; for reverse, run KHAREJ setup and paste it there instead."
  ok "Link OK — kharej endpoint $ENDPOINT, transport $TRANSPORT (carrier $CARRIER)."
  echo >&2; info "This Iran server's IP addresses:"; show_ips
  read -rp "Dial out FROM which local IP? (Enter = automatic): " EGRESSIP </dev/tty
  mkdir -p "$(dirname "$CFG")"
  if [ "$TRANSPORT" = "tcp" ]; then
    read -rp "IP that USERS connect to on this server (Enter = all IPs): " USERIP </dev/tty
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443): " PORTS </dev/tty
    [ -n "$PORTS" ] || die "at least one port is required"
    for p in ${PORTS//,/ }; do
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": false, "udp": $UDP,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP", "user_listen_ip": "$USERIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    ok "IRAN ready (direct, tcp). Users connect on port(s): $PORTS"
  elif [ "$TRANSPORT" = "tun" ]; then
    # Backhaul-style L3 tunnel over multi-link TLS (l3mtcp). Iran is the TLS
    # client here (validates the kharej's domain as SNI). MTU comes from the link
    # so both sides match; the interface name is a local choice.
    [ -n "$MTU" ] || die "this link has no MTU field — regenerate it on the kharej with the new installer."
    ask_tun_params
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "l3mtcp", "reverse": false,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": $MTU,
  "shared_key": "$SHARED",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    ok "IRAN ready (direct, tun / L3 over multi-link TLS)."
    info "L3 tunnel on $TUNIF once up: this iran = 10.77.0.1, kharej = 10.77.0.2 (MTU $MTU)."
    info "Route the traffic you want tunneled toward 10.77.0.2 over $TUNIF."
  else
    # udp/auto is a TUN IP tunnel on hs0 (not a port forwarder); no user ports.
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": false,
  "addr": "$ENDPOINT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED", "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    ok "IRAN ready (direct, $TRANSPORT / UDP+FEC)."
    info "An IP tunnel is up on hs0 (iran 10.77.0.1, kharej 10.77.0.2)."
    info "Route panel/user traffic over hs0 (e.g. to 10.77.0.2)."
    [ "$TRANSPORT" = "auto" ] && info "auto: if UDP is blocked or too lossy, it falls back to TCP silently."
  fi
  info "Backhaul is untouched. Status/logs any time:  bash install.sh → 4"
}

# iran_listener: reverse edge. Iran listens for the kharej (which dials in) and
# generates the link. For tcp it also opens the user ports; for udp/auto it is a
# TUN IP tunnel on hs0.
iran_listener(){
  read -rp "Tunnel port to LISTEN on (kharej dials it) [2096]: " TPORT </dev/tty; TPORT=${TPORT:-2096}
  local SHARED; SHARED=$(openssl rand -hex 32)
  local LMTU=""
  mkdir -p "$(dirname "$CFG")"

  if [ "$TRANSPORT" = "tcp" ]; then
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    read -rp "IP that USERS connect to on this server (Enter = all IPs): " USERIP </dev/tty
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443): " PORTS </dev/tty
    [ -n "$PORTS" ] || die "at least one port is required"
    for p in ${PORTS//,/ }; do
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    read -rp "Domain for THIS iran server (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required (the kharej validates it as the TLS name)"
    echo >&2
    echo "  TLS mode:  1) mtcp (recommended)  2) l3mtcp  3) tls" >&2
    read -rp "Choose [1]: " M </dev/tty
    case "${M:-1}" in 1) CARRIER=mtcp ;; 2) CARRIER=l3mtcp ;; 3) CARRIER=tls ;; *) die "invalid mode" ;; esac
    read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
    case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    # Reverse: THIS iran server is the TLS server, so the cert is obtained HERE
    # (in direct it would be on the kharej). The kharej dials in and validates
    # this domain as the TLS name, so it must match the cert.
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true, "udp": $UDP,
  "addr": "0.0.0.0:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN ready (reverse, tcp). Users connect on port(s): $PORTS"
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse tun: iran LISTENS and is the TLS server, so the cert lives HERE.
    # The kharej dials in. L3 routed interface, no user ports.
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    read -rp "Domain for THIS iran server (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required (the kharej validates it as the TLS name)"
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; CARRIER=l3mtcp; UDP=false
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "l3mtcp", "reverse": true,
  "addr": "0.0.0.0:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": $TUNMTU,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN ready (reverse, tun / L3 over multi-link TLS)."
    info "L3 tunnel on $TUNIF once up: this iran = 10.77.0.1, kharej = 10.77.0.2 (MTU $LMTU)."
  else
    udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    [ "$TRANSPORT" = "auto" ] && { port_free "$TPORT" || die "auto also needs TCP port $TPORT free — pick another."; }
    CARRIER=$(transport_to_carrier "$TRANSPORT"); DOMAIN="-"; UDP=false
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true,
  "addr": "0.0.0.0:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN ready (reverse, $TRANSPORT / UDP+FEC)."
    info "An IP tunnel is up on hs0 (iran 10.77.0.1, kharej 10.77.0.2). Route traffic over hs0."
  fi
  # panel is set on the kharej side; leave it blank in the link.
  show_link "$PUBIP:$TPORT" "$DOMAIN" "$SHARED" "-" "$CARRIER" "$UDP" "$TRANSPORT" "reverse" "$LMTU"
  info "On the Kharej server: bash install.sh → 1 (Kharej) → direction 'reverse' → paste the link."
}

# ---------- uninstall & status ----------------------------------------------
uninstall(){
  info "Removing hs2…"
  systemctl disable --now hs2 2>/dev/null || true
  sleep 1
  pkill -TERM -x hs2 2>/dev/null || true; sleep 1; pkill -KILL -x hs2 2>/dev/null || true
  # Delete the tunnel interface. Usually hs0, but tun mode may have renamed it, so
  # also read the name from the config before deleting it.
  local IFACE=""
  [ -f "$CFG" ] && IFACE=$(grep -o '"iface"[[:space:]]*:[[:space:]]*"[^"]*"' "$CFG" 2>/dev/null | sed 's/.*"\([^"]*\)"$/\1/')
  [ -n "$IFACE" ] && ip link del "$IFACE" 2>/dev/null || true
  ip link del hs0 2>/dev/null || true
  rm -f "$SVC" "$CFG" /etc/sysctl.d/99-hs2.conf; systemctl daemon-reload
  ok "hs2 removed (service stopped, hs0 deleted). Backhaul untouched."
}

status(){
  hr; systemctl status hs2 --no-pager 2>/dev/null | head -12 || true; hr
  info "Recent log:"
  journalctl -u hs2 -n 25 --no-pager 2>/dev/null || true
  echo >&2
  if ip -br addr show hs0 >/dev/null 2>&1; then
    ok "Tunnel interface hs0: $(ip -br addr show hs0 | awk '{print $3}')"
  else
    warn "Tunnel interface hs0 is not up."
  fi
}

# Upgrade in place: new binary + kernel tuning, same config and hs2:// link.
upgrade(){
  [ -f "$CFG" ] || die "hs2 is not installed on this server ($CFG missing). Run without 'upgrade' to install."
  hr; info "Upgrading hs2 (config and link stay the same)"; hr
  install_prereqs
  install_binary
  # Configs written by the v2 installer use 4 links, too few against
  # per-connection throttling. Move them to the new defaults, but only if the
  # line is exactly the old default (hand-edited values are left alone).
  if grep -q '"min_links": 4, "max_links": 16, "per_link": 50,' "$CFG"; then
    sed -i "s/\"min_links\": 4, \"max_links\": 16, \"per_link\": 50,/\"min_links\": $LINK_MIN, \"max_links\": $LINK_MAX, \"per_link\": $LINK_PER,/" "$CFG"
    ok "Link pool updated to $LINK_MIN-$LINK_MAX links."
  fi
  systemctl restart hs2
  sleep 2
  if systemctl is-active --quiet hs2; then
    ok "hs2 upgraded and running. Upgrade the OTHER server too (both sides must match)."
    info "Watch the log:  journalctl -u hs2 -f"
  else
    err "hs2 failed to start after upgrade. Last log:"
    journalctl -u hs2 -n 20 --no-pager >&2
    exit 1
  fi
}

# Non-interactive: bash install.sh upgrade   (or: curl … | bash -s upgrade)
if [ "${1:-}" = "upgrade" ]; then upgrade; exit 0; fi

# ---------- menu -------------------------------------------------------------
echo >&2
_c '1;36' "╔══════════════════════════════════════════╗"
_c '1;36' "║   hs2 v3 — DPI-resistant tunnel           ║"
_c '1;36' "║   runs alongside Backhaul                 ║"
_c '1;36' "╚══════════════════════════════════════════╝"
echo >&2
echo "  1) Kharej  (foreign server — panel side)" >&2
echo "  2) Iran    (opens user ports → panel)" >&2
echo "  3) Uninstall hs2" >&2
echo "  4) Status / logs" >&2
echo "  5) Upgrade (new binary, keep config)" >&2
echo >&2
read -rp "Choose [1-5]: " CH </dev/tty
case "$CH" in
  1) setup_kharej ;;
  2) setup_iran ;;
  3) uninstall ;;
  4) status ;;
  5) upgrade ;;
  *) die "invalid choice" ;;
esac

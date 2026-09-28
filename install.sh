#!/usr/bin/env bash
# ============================================================================
#  hs2 — DPI-resistant tunnel (layer: L3-GRE over multi-link TLS)
#  Runs ALONGSIDE Backhaul without touching it.
#      bash install.sh            (menu)
#      bash install.sh upgrade    (update an existing install in place)
# ============================================================================
set -euo pipefail

BIN=/usr/local/bin/hs2
REPO_RAW="${HS2_REPO_RAW:-https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/claude/amazing-meitner-vl4b5d}"
CFG=/etc/hs2/config.json
SVC=/etc/systemd/system/hs2.service
TUN_SUBNET_IRAN="10.77.0.1/30"
TUN_SUBNET_KHAREJ="10.77.0.2/30"
TUN_PEER_IRAN="10.77.0.2"
TUN_PEER_KHAREJ="10.77.0.1"

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
  "$tmp" version 2>/dev/null | grep -q "l3mtcp" \
    || { rm -f "$tmp"; die "binary is outdated/corrupt (no l3mtcp). Re-download hs2-linux-amd64."; }
  systemctl stop hs2 2>/dev/null || true
  install -m755 "$tmp" "$BIN"; rm -f "$tmp"
  info "sha256: $(sha256sum "$BIN" | cut -c1-16)…"
  "$BIN" version 2>/dev/null | grep -q "l3mtcp"     || die "binary is outdated/corrupt (no l3mtcp). Re-download hs2-linux-amd64."
  ok "Installed $("$BIN" version 2>/dev/null)"
}

write_service(){
  local role="$1"
  cat > "$SVC" <<EOF
[Unit]
Description=hs2 DPI-resistant tunnel (L3-GRE, $role)
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
get_cert(){
  local domain="$1"
  command -v certbot >/dev/null 2>&1 || {
    info "Installing certbot…"
    apt-get install -y -q certbot >/dev/null 2>&1 || die "could not install certbot"
  }
  if [ -f "/etc/letsencrypt/live/$domain/fullchain.pem" ]; then
    info "Using existing certificate for $domain"
  elif port_free 80; then
    info "Getting Let's Encrypt certificate (standalone on port 80)…"
    certbot certonly --standalone -d "$domain" --non-interactive --agree-tos \
      --register-unsafely-without-email --deploy-hook "systemctl restart hs2" \
      >/dev/null 2>&1 || die "certbot failed — check that $domain points here and port 80 is open."
  else
    die "port 80 is busy and no existing cert for $domain. Free port 80 or place a cert at /etc/letsencrypt/live/$domain/."
  fi
  printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
}

# ---------- KHAREJ (foreign server) ------------------------------------------
setup_kharej(){
  hr; info "KHAREJ setup (foreign server — where the panel lives)"; hr
  install_prereqs
  install_binary

  echo >&2; info "This server's IP addresses:"; show_ips
  local defip; defip=$(first_public_ip)
  read -rp "Public IP of THIS kharej server [$defip]: " PUBIP </dev/tty; PUBIP=${PUBIP:-$defip}
  [ -n "$PUBIP" ] || die "public IP required"

  read -rp "Domain (its A record must point to $PUBIP): " DOMAIN </dev/tty
  [ -n "$DOMAIN" ] || die "domain required"

  read -rp "Tunnel port (clients never see this) [2096]: " TPORT </dev/tty; TPORT=${TPORT:-2096}
  port_free "$TPORT" || die "port $TPORT is already in use — pick another."

  read -rp "Panel inbound address on this server [127.0.0.1:8443]: " PANEL </dev/tty
  PANEL=${PANEL:-127.0.0.1:8443}

  local certpair CERT KEY
  certpair=$(get_cert "$DOMAIN"); CERT=${certpair%%|*}; KEY=${certpair##*|}

  local SHARED; SHARED=$(openssl rand -hex 32)

  mkdir -p "$(dirname "$CFG")"
  cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "l3mtcp",
  "addr": "$PUBIP:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "expose": "$PANEL"
}
EOF
  chmod 600 "$CFG"
  write_service kharej
  start_service kharej

  # auto-link for the Iran side: endpoint|domain|shared|panel
  local LINK; LINK=$(encode_link "$PUBIP:$TPORT|$DOMAIN|$SHARED|$PANEL")
  echo >&2; hr
  ok "KHAREJ ready. Copy this SETUP LINK to the Iran server:"
  _c '1;33' "hs2://$LINK"
  hr
  info "On the Iran server: bash install.sh → choose 2 → paste the link."
}

# ---------- IRAN -------------------------------------------------------------
setup_iran(){
  hr; info "IRAN setup (opens user ports, forwards to the panel over the tunnel)"; hr
  install_prereqs
  install_binary

  read -rp "Paste the hs2:// setup link from the kharej server: " RAW </dev/tty
  RAW=${RAW#hs2://}
  local DEC; DEC=$(decode_link "$RAW") || die "invalid link"
  local ENDPOINT DOMAIN SHARED PANEL
  IFS='|' read -r ENDPOINT DOMAIN SHARED PANEL <<< "$DEC"
  [ -n "$ENDPOINT" ] && [ -n "$SHARED" ] || die "link is missing fields"
  ok "Link OK — kharej endpoint $ENDPOINT, domain $DOMAIN"

  echo >&2; info "This Iran server's IP addresses:"; show_ips
  read -rp "Dial out FROM which local IP? (Enter = automatic): " EGRESSIP </dev/tty

  read -rp "IP that USERS connect to on this server (Enter = all IPs): " USERIP </dev/tty

  read -rp "User port(s) to open here, comma-separated (e.g. 8443,443): " PORTS </dev/tty
  [ -n "$PORTS" ] || die "at least one port is required"
  for p in ${PORTS//,/ }; do
    port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
  done

  info "Multi-link auto-scales between 4 and 16 parallel links by load."

  mkdir -p "$(dirname "$CFG")"
  cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "l3mtcp",
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL",
  "min_links": 4, "max_links": 16, "per_link": 50,
  "bind_local_ip": "$EGRESSIP",
  "user_listen_ip": "$USERIP"
}
EOF
  chmod 600 "$CFG"
  write_service iran
  start_service iran

  echo >&2; hr
  ok "IRAN ready. Users connect to this server on port(s): $PORTS"
  info "Backhaul is untouched (its own ports/subnet)."
  info "Check status any time:  bash install.sh → 4"
}

# ---------- uninstall & status ----------------------------------------------
uninstall(){
  info "Removing hs2…"
  systemctl disable --now hs2 2>/dev/null || true
  sleep 1
  pkill -TERM -x hs2 2>/dev/null || true; sleep 1; pkill -KILL -x hs2 2>/dev/null || true
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
_c '1;36' "║   hs2 — DPI-resistant tunnel (L3-GRE)     ║"
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

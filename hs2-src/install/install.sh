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
# ---------- input validation ---------------------------------------------------
# Every value typed by the user ends up in JSON and in listen/dial addresses, so
# each one is checked before use; a bad answer is asked again, not written.
is_port(){ [[ "$1" =~ ^[1-9][0-9]{0,4}$ ]] && [ "$1" -le 65535 ]; }
is_ipv4(){
  [[ "$1" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  local o; for o in "${BASH_REMATCH[@]:1}"; do
    [[ "$o" =~ ^(0|[1-9][0-9]{0,2})$ ]] && [ "$o" -le 255 ] || return 1
  done
}
is_ipv6(){
  local s=$1 g n=0
  [[ "$s" == *:* && "$s" =~ ^[0-9A-Fa-f:]+$ && ${#s} -le 39 ]] || return 1
  [[ "$s" == *:::* ]] && return 1
  [ "$(grep -o '::' <<< "$s" | wc -l)" -le 1 ] || return 1
  local IFS=:
  for g in $s; do
    [ -z "$g" ] && continue
    [ ${#g} -le 4 ] || return 1
    n=$((n+1))
  done
  if [[ "$s" == *::* ]]; then [ "$n" -le 7 ]; else [ "$n" -eq 8 ]; fi
}
is_ip(){ is_ipv4 "$1" || is_ipv6 "$1"; }
is_domain(){ [ ${#1} -le 253 ] && [[ "$1" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]]; }
is_key(){ [[ "$1" =~ ^[0-9a-f]{64}$ ]]; }
# host:port, or [ipv6]:port; host = IPv4, IPv6 (bracketed), domain or localhost
is_hostport(){
  local h p
  if [[ "$1" =~ ^\[([^]]+)\]:([0-9]+)$ ]]; then
    h=${BASH_REMATCH[1]}; p=${BASH_REMATCH[2]}; is_ipv6 "$h" || return 1
  elif [[ "$1" =~ ^([^:]+):([0-9]+)$ ]]; then
    h=${BASH_REMATCH[1]}; p=${BASH_REMATCH[2]}
    is_ipv4 "$h" || is_domain "$h" || [ "$h" = localhost ] || return 1
  else
    return 1
  fi
  is_port "$p"
}
# join host and port, bracketing IPv6
hostport(){ if [[ "$1" == *:* ]]; then printf '[%s]:%s' "$1" "$2"; else printf '%s:%s' "$1" "$2"; fi; }
is_local_ip(){ ip -br addr 2>/dev/null | tr -s ' ' '\n' | sed 's#/.*##' | grep -qxF "$1"; }
opt_local_ip(){ [ -z "$1" ] || { is_ip "$1" && is_local_ip "$1"; }; }

port_free(){ is_port "$1" && ! ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }
udp_free(){ is_port "$1" && ! ss -Hlun "sport = :$1" 2>/dev/null | grep -q .; }

# ask VAR "prompt" default check "why it was rejected"
# Whitespace is removed from the answer; up to three tries.
ask(){
  local __var=$1 prompt=$2 def=$3 check=$4 why=$5 ans i
  for i in 1 2 3; do
    read -rp "$prompt" ans </dev/tty || ans=""
    ans=${ans:-$def}
    ans=$(printf '%s' "$ans" | tr -d '[:space:]')
    if "$check" "$ans"; then printf -v "$__var" '%s' "$ans"; return 0; fi
    warn "$why"
  done
  die "too many invalid answers; run the installer again"
}

# user ports: comma-separated, each valid, no duplicates, free (TCP, and UDP
# too when UDP forwarding is on)
ports_ok(){
  local list=$1 p seen=","
  [ -n "$list" ] || return 1
  local IFS=,
  for p in $list; do
    is_port "$p" || { warn "  '$p' is not a port number (1-65535)"; return 1; }
    [[ "$seen" == *",$p,"* ]] && { warn "  port $p is listed twice"; return 1; }
    seen="$seen$p,"
    port_free "$p" || { warn "  TCP port $p is already in use (Backhaul or panel?)"; return 1; }
    if [ "${UDP:-false}" = true ]; then
      udp_free "$p" || { warn "  UDP port $p is already in use"; return 1; }
    fi
  done
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
  ask PUBIP "Public IP of THIS kharej server [$defip]: " "$defip" is_ip \
    "not a valid IPv4/IPv6 address (example: 203.0.113.5)"
  # On NAT'd clouds the public IP is not on any interface and cannot be bound;
  # then listen on all addresses and still hand out the public IP in the link.
  local LISTEN_HOST=$PUBIP
  if ! is_local_ip "$PUBIP"; then
    warn "$PUBIP is not assigned to this server (NAT?) — listening on all addresses."
    if is_ipv6 "$PUBIP"; then LISTEN_HOST="::"; else LISTEN_HOST="0.0.0.0"; fi
  fi

  ask DOMAIN "Domain (its A record must point to $PUBIP): " "" is_domain \
    "not a valid domain name (example: vpn.example.com)"
  if command -v getent >/dev/null 2>&1; then
    getent ahosts "$DOMAIN" 2>/dev/null | awk '{print $1}' | grep -qxF "$PUBIP" \
      || warn "$DOMAIN does not resolve to $PUBIP (yet) — the certificate step needs it to."
  fi

  tport_ok(){ port_free "$1"; }
  ask TPORT "Tunnel port (clients never see this) [2096]: " 2096 tport_ok \
    "not a port number (1-65535), or it is already in use"

  panel_ok(){
    is_hostport "$1" || return 1
    # the panel cannot be on the tunnel port of this server
    [ "${1##*:}" != "$TPORT" ] || { warn "  the panel port must differ from the tunnel port $TPORT"; return 1; }
  }
  ask PANEL "Panel inbound address on this server [127.0.0.1:8443]: " 127.0.0.1:8443 panel_ok \
    "want host:port, e.g. 127.0.0.1:8443 (IPv6 as [::1]:8443)"

  echo >&2
  echo "  Tunnel mode:" >&2
  echo "    1) mtcp    — multi-link, fastest (recommended)" >&2
  echo "    2) l3mtcp  — mtcp + tunnel IPs 10.77.0.1/2 on hs0 (ping, non-TCP)" >&2
  echo "    3) tls     — a single link + hs0" >&2
  mode_ok(){ [[ "$1" =~ ^[123]$ ]]; }
  ask M "Choose [1]: " 1 mode_ok "choose 1, 2 or 3"
  case "$M" in 1) CARRIER=mtcp ;; 2) CARRIER=l3mtcp ;; 3) CARRIER=tls ;; esac
  read -rp "Also forward UDP on the user ports (e.g. for Hysteria/WireGuard)? [y/N]: " U </dev/tty
  case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac

  local certpair CERT KEY
  certpair=$(get_cert "$DOMAIN"); CERT=${certpair%%|*}; KEY=${certpair##*|}

  local SHARED; SHARED=$(openssl rand -hex 32)

  mkdir -p "$(dirname "$CFG")"
  cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER",
  "addr": "$(hostport "$LISTEN_HOST" "$TPORT")",
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

  # auto-link for the Iran side: endpoint|domain|shared|panel|mode|udp
  local LINK; LINK=$(encode_link "$(hostport "$PUBIP" "$TPORT")|$DOMAIN|$SHARED|$PANEL|$CARRIER|$UDP")
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
  local ENDPOINT DOMAIN SHARED PANEL CARRIER UDP
  IFS='|' read -r ENDPOINT DOMAIN SHARED PANEL CARRIER UDP <<< "$DEC"
  CARRIER=${CARRIER:-mtcp}; UDP=${UDP:-false}   # links from older kharej installs
  local broken="the setup link is incomplete or damaged; copy the whole hs2:// line again"
  is_hostport "$ENDPOINT" || die "$broken (endpoint)"
  is_domain "$DOMAIN"     || die "$broken (domain)"
  is_key "$SHARED"        || die "$broken (key)"
  is_hostport "$PANEL"    || die "$broken (panel)"
  case "$CARRIER" in mtcp|l3mtcp|tls) ;; *) die "$broken (mode)" ;; esac
  case "$UDP" in true|false) ;; *) die "$broken (udp)" ;; esac
  ok "Link OK — kharej endpoint $ENDPOINT, domain $DOMAIN, mode $CARRIER, udp $UDP"

  echo >&2; info "This Iran server's IP addresses:"; show_ips
  ask EGRESSIP "Dial out FROM which local IP? (Enter = automatic): " "" opt_local_ip \
    "must be one of this server's IP addresses listed above, or empty"
  ask USERIP "IP that USERS connect to on this server (Enter = all IPs): " "" opt_local_ip \
    "must be one of this server's IP addresses listed above, or empty"
  ask PORTS "User port(s) to open here, comma-separated (e.g. 8443,443): " "" ports_ok \
    "enter free port numbers separated by commas"

  info "Links auto-scale between $LINK_MIN and $LINK_MAX by load."

  mkdir -p "$(dirname "$CFG")"
  cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "udp": $UDP,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
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

# Tests source this file with HS2_LIB=1 to reach the helpers without the menu.
if [ "${HS2_LIB:-0}" = 1 ]; then return 0 2>/dev/null || exit 0; fi

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

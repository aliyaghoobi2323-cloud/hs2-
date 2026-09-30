#!/usr/bin/env bash
# ============================================================================
#  hs2 — DPI-resistant tunnel (stream mode over multi-link TLS)
#  Runs ALONGSIDE Backhaul without touching it.
#      bash install.sh            (menu)
#      bash install.sh upgrade    (update an existing install in place)
#      bash install.sh backup     (save config+cert+binary to /root/hs2-backups)
#      bash install.sh restore [file]  (roll back to a backup; newest by default)
#      bash install.sh manage     (tunnel manager: start/stop/restart/edit/logs)
#      hs2-menu                   (this menu, installed locally by setup/upgrade)
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
# Adaptive parallel-link envelope written into new configs. The pool is NOT
# fixed at these numbers: hs2 sizes it continuously between LINK_MIN and LINK_MAX
# from the live user count and measured throughput (see engine/autopilot.go), and
# grows a link roughly per LINK_PER users. See BUILD.md.
LINK_MIN=2
LINK_MAX=32
LINK_PER=8

# How the L3 tun (transport "tun") crosses the wire. Set by ask_tun_encap.
#   udp/icmp/gre/ipip/ipx = carrier "dgtun" (a routed TUN over a datagram pool)
#   tcp                   = carrier "l3mtcp" (mtcp pool + tun) or "tls" (one link + tun)
# TUN_PROTO is the raw IP protocol number for the "ipx" encapsulation only.
TUN_ENCAP=udp
TUN_PROTO=""

# ---------- pretty output (all to stderr so $(...) captures only real values) --
_c(){ printf '\033[%sm%s\033[0m\n' "$1" "$2" >&2; }
info(){ _c '1;34' "→ $1"; }
ok(){   _c '1;32' "✓ $1"; }
warn(){ _c '1;33' "! $1"; }
err(){  _c '1;31' "✗ $1"; }
die(){  err "$1"; HS2_DIED=1; exit 1; }
# bail: exit 1 after an error that has already been explained on screen.
bail(){ HS2_DIED=1; exit 1; }
hr(){   _c '0;36' "────────────────────────────────────────────"; }

[ "$(id -u)" = 0 ] || die "Please run as root."

# Safety net. Installing the binary stops hs2 (HS2_STOPPED=1). If the script
# ends for ANY reason before hs2 is running again — an error, an unexpected
# set -e stop, Ctrl+C at a prompt — start it again with whatever config is in
# place, so an upgrade or re-run can never leave the tunnel down. An exit that
# was not a deliberate `die` also says exactly which command stopped it.
HS2_STOPPED=0
HS2_DIED=0
on_exit(){
  local rc=$? cmd=$BASH_COMMAND
  if [ "$rc" != 0 ] && [ "$HS2_DIED" != 1 ]; then
    err "The installer stopped unexpectedly (status $rc) at: $cmd"
    err "Please send this line to the developer."
  fi
  if [ "$HS2_STOPPED" = 1 ] && [ -f "$CFG" ] && [ -f "$SVC" ] \
     && [ "$(systemctl is-active hs2 2>/dev/null || true)" != active ]; then
    warn "hs2 was stopped for the update and is not running — starting it again with the current config…"
    systemctl start hs2 2>/dev/null || true
    sleep 2
    if [ "$(systemctl is-active hs2 2>/dev/null || true)" = active ]; then ok "hs2 is running again."
    else err "hs2 could not be started — see: journalctl -u hs2 -n 40 --no-pager"; fi
  fi
  return 0
}
trap on_exit EXIT

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

# Every IPv4 on every interface (ip -br prints only the first address of each
# interface, which hid secondary IPs on multi-IP servers).
local_ips(){ ip -4 -o addr show 2>/dev/null | awk '$2!="lo"{print $4}' | sed 's#/.*##'; }
show_ips(){ local_ips | sed 's/^/   /' >&2; }
first_public_ip(){ local_ips | head -1; }

# ip_is_local reports whether an IPv4 address is assigned to a local interface.
ip_is_local(){ ip -4 -o addr show 2>/dev/null | awk '{print $4}' | sed 's#/.*##' | grep -x "$1" >/dev/null; }

# ask_bind_ip sets BINDADDR: the local IP this server LISTENS on. A specific IP
# lets a multi-IP server dedicate one address to the tunnel; it is validated to
# actually exist locally, so a non-local IP (e.g. a NAT public IP that is not on
# any interface) is rejected here with a clear message instead of failing at
# runtime with "cannot assign requested address". Enter = all interfaces.
ask_bind_ip(){
  local n; n=$(local_ips | wc -l)
  if [ "$n" -le 1 ]; then
    BINDADDR="0.0.0.0"
    info "Only one local IP here ($(first_public_ip)) — listening on it (all interfaces)."
    return 0
  fi
  # Default to the public IP given above when it is one of ours.
  local def=""; [ -n "${PUBIP:-}" ] && ip_is_local "$PUBIP" && def="$PUBIP"
  echo >&2; info "This server has several IPs:"; show_ips
  while :; do
    read -rp "LISTEN on which local IP? [${def:-Enter = all}]: " BINDIP </dev/tty
    BINDIP=${BINDIP:-$def}
    if [ -z "$BINDIP" ]; then BINDADDR="0.0.0.0"; ok "Listening on all interfaces."; return 0; fi
    if ip_is_local "$BINDIP"; then BINDADDR="$BINDIP"; ok "Listening on $BINDIP."; return 0; fi
    warn "$BINDIP is not on this server. Pick one from the list, or Enter for all."
    warn "(A public IP that your provider NATs to this server is not local — keep it as the"
    warn " 'Public IP' above and press Enter here.)"
  done
}

# ask_egress_ip sets EGRESSIP: the local source IP this server DIALS from. A
# specific IP is validated to exist locally (same reason as ask_bind_ip). The
# rp_filter=2 tuning above lets a non-default source IP's return traffic through.
ask_egress_ip(){
  EGRESSIP=""
  local n; n=$(local_ips | wc -l)
  if [ "$n" -le 1 ]; then
    info "Only one local IP here ($(first_public_ip)) — tunnel connections leave from it."
    return 0
  fi
  echo >&2; info "This server has several IPs:"; show_ips
  while :; do
    read -rp "Dial out FROM which local IP? (use the one that is NOT filtered; Enter = automatic): " EGRESSIP </dev/tty
    if [ -z "$EGRESSIP" ]; then warn "Automatic: the kernel picks the source (usually the first IP)."; return 0; fi
    if ip_is_local "$EGRESSIP"; then ok "Dialing from $EGRESSIP."; return 0; fi
    warn "$EGRESSIP is not on this server. Pick one from the list above."
  done
}

# ask_user_ip sets USERIP (user_listen_ip): the local IP the USER ports open on.
# Enter keeps every address — all IPv4 AND IPv6 — which on a multi-IP server
# is easy to do by accident, so there the IPs are listed and the choice spelled
# out. A typed IP is validated like ask_bind_ip (a typo would otherwise only
# show up as a failed start). 'all' or 0.0.0.0 also mean every address.
ask_user_ip(){
  local n; n=$(local_ips | wc -l)
  if [ "$n" -gt 1 ]; then echo >&2; info "IPs on this server (the user ports can open on one of them, or on all):"; show_ips; fi
  while :; do
    read -rp "IP that USERS connect to on this server (Enter = ALL IPs, IPv4 and IPv6): " USERIP </dev/tty
    case "$USERIP" in
      ''|all|ALL|0.0.0.0|'*') USERIP=""
        [ "$n" -gt 1 ] && info "User ports open on all $n IPv4 addresses (and IPv6)."
        return 0 ;;
    esac
    if ip_is_local "$USERIP"; then ok "User ports open on $USERIP only."; return 0; fi
    warn "$USERIP is not on this server. Pick one from the list, or Enter for all."
  done
}

install_prereqs(){
  info "Installing prerequisites…"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q </dev/null >/dev/null 2>&1 || true
  apt-get install -y -q iproute2 iptables curl ca-certificates </dev/null >/dev/null 2>&1 || true
  # nftables: the icmp tunnel drops only the kernel's replies to ITS OWN packets
  # with an nft rule (iptables u32 as a fallback), so the server keeps answering
  # normal ping. ping: the installer proves the tunnel carries packets. Asked
  # separately so a missing package name can never block the ones above.
  apt-get install -y -q nftables iputils-ping </dev/null >/dev/null 2>&1 || true
  # tun module
  modprobe tun 2>/dev/null || true
  tune_kernel
  ok "Prerequisites ready."
}

# Kernel tuning now lives IN the binary: it is sized to the server's RAM and CPU
# cores and re-applied on every start (see `hs2 tune` and tune/tune.go), so there
# is a single source of truth and a resized VPS is picked up automatically. Here
# we only make BBR available early (load the module and persist it for boot) and
# remove the old static sysctl file so it cannot be a stale second source.
tune_kernel(){
  modprobe tcp_bbr 2>/dev/null || true
  echo tcp_bbr > /etc/modules-load.d/hs2.conf 2>/dev/null || true
  if [ -f /etc/sysctl.d/99-hs2.conf ]; then
    rm -f /etc/sysctl.d/99-hs2.conf
    info "Kernel tuning is now applied by hs2 at runtime (RAM/CPU-aware). Removed the old static file."
  fi
  ok "BBR available. hs2 applies RAM/CPU-aware tuning at startup (see 'hs2-menu' → tunnel → Tuning)."
}

# verify_download FILE: check a downloaded binary against the sha256 published
# next to it (hs2-linux-amd64.sha256). It catches a truncated or altered
# download — a middlebox, a broken proxy, a CDN hiccup. It is NOT a signature:
# whoever can change the repository can change both files, so protect the
# GitHub account (2FA), and pin a reviewed commit with
# HS2_REPO_RAW=https://raw.githubusercontent.com/<owner>/<repo>/<commit> when that
# matters. A repository without the .sha256 file (older) only warns.
verify_download(){ # file [url-suffix]
  local want got
  want=$(curl -fsSL --connect-timeout 10 --retry 2 "$REPO_RAW/hs2-linux-amd64.sha256${2:-}" 2>/dev/null | awk 'NR==1{print $1}' || true)
  case "$want" in
    [0-9a-f]*) [ ${#want} = 64 ] || want="" ;;
    *) want="" ;;
  esac
  if [ -z "$want" ]; then
    warn "No published sha256 to check the download against (hs2-linux-amd64.sha256 missing) — relying on the version check only."
    return 0
  fi
  got=$(sha256sum "$1" | cut -d' ' -f1)
  if [ "$got" = "$want" ]; then ok "sha256 matches the published hash."; return 0; fi
  err "sha256 mismatch: published $want, downloaded $got"
  return 1
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
    if ! verify_download "$tmp"; then
      # Right after a release the CDN can serve the new binary with the old hash
      # (or the reverse) for a few minutes: fetch both once more past the cache
      # before calling the download bad.
      local q="?v=$(date +%s)"
      warn "Fetching the binary and its hash once more (bypassing the CDN cache)…"
      if ! curl -fL --connect-timeout 10 --retry 2 -o "$tmp" "$REPO_RAW/hs2-linux-amd64$q" 2>/dev/null \
         || ! verify_download "$tmp" "$q"; then
        rm -f "$tmp"
        die "the downloaded binary does not match its published sha256 — NOT installed. Run again in a few minutes; if it keeps failing, something on the path is altering the download."
      fi
    fi
  elif [ -f "$d/hs2-linux-amd64" ] || [ -f "./hs2-linux-amd64" ]; then
    local f="$d/hs2-linux-amd64"; [ -f "$f" ] || f="./hs2-linux-amd64"
    warn "GitHub unreachable — using local $f (make sure it is the NEW one)."
    cp "$f" "$tmp"
  elif [ -x "$BIN" ] && "$BIN" version 2>/dev/null | grep -q "hs2 v3"; then
    # Re-running setup (e.g. from hs2-menu) on a server that cannot reach
    # GitHub: the binary already installed is good enough to build a tunnel.
    warn "GitHub unreachable — keeping the hs2 already installed on this server."
    warn "(Run 'Upgrade' later when GitHub is reachable, on BOTH servers.)"
    cp "$BIN" "$tmp"
  else
    rm -f "$tmp"
    die "download failed. On the Iran server, copy hs2-linux-amd64 from the kharej server into $(pwd) and run again."
  fi
  chmod 755 "$tmp"
  "$tmp" version 2>/dev/null | grep -q "hs2 v3" \
    || { rm -f "$tmp"; die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."; }
  [ -f "$SVC" ] && HS2_STOPPED=1
  systemctl stop hs2 2>/dev/null || true
  install -m755 "$tmp" "$BIN"; rm -f "$tmp"
  # The full hash: compare it between the two servers (the Iran side may have
  # been given a copy by hand).
  info "sha256: $(sha256sum "$BIN" | cut -d' ' -f1)"
  "$BIN" version 2>/dev/null | grep -q "hs2 v3"     || die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."
  ok "Installed $("$BIN" version 2>/dev/null)"
  install_self
}

# unit_unmask UNIT: a MASKED unit is a symlink to /dev/null (in /etc, or in /run
# for a runtime mask). Writing the unit file then goes nowhere or is overridden,
# `enable`/`restart` fail, and an instance started BEFORE the mask keeps running
# the OLD config while still reporting "active" — so a new config would silently
# never load. This installer never masks hs2; if something else did, undo it
# loudly so the config we write is the config that runs.
unit_unmask(){ # unit
  local u="$1" f="/etc/systemd/system/$1.service"
  case "$(systemctl is-enabled "$u" 2>/dev/null || true)" in
    masked*) ;;
    *) return 0 ;;
  esac
  warn "$u was MASKED (by something outside this installer) — unmasking it so the new config actually runs."
  systemctl unmask "$u" >/dev/null 2>&1 || true
  systemctl unmask --runtime "$u" >/dev/null 2>&1 || true
  if [ -L "$f" ] && [ "$(readlink -f "$f" 2>/dev/null)" = /dev/null ]; then rm -f "$f"; fi
  systemctl daemon-reload
}

# restart_unit UNIT: (re)start it and prove the NEW process is the one running —
# active with a stable PID (tm_healthy) AND a different PID than before. A
# restart that did not happen (the old instance still running, e.g. a unit that
# was masked while it ran) is a failure here, never a false "running".
restart_unit(){ # unit
  local u="$1" before after
  unit_unmask "$u"
  before=$(tm_prop "$u" MainPID)
  systemctl restart "$u" 2>/dev/null || return 1
  tm_healthy "$u" || return 1
  after=$(tm_prop "$u" MainPID)
  [ "${after:-0}" != 0 ] && [ "$after" != "${before:-0}" ]
}

write_service(){
  local role="$1"
  unit_unmask hs2   # before writing: a masked unit file is a /dev/null symlink
  # Built to come back on its own after a reboot or a crash:
  #  - enabled for multi-user.target (start_service/upgrade run `enable`)
  #  - waits for network-online, but never depends on it: if the IP is not up
  #    yet it simply fails and is restarted 3 s later
  #  - StartLimitIntervalSec=0: systemd never gives up restarting it
  #  - loads the tun module first (the tunnel interface needs /dev/net/tun)
  # Capabilities: the unit sets no User=, DynamicUser=, NoNewPrivileges= or
  # CapabilityBoundingSet=, so it runs as root with the FULL capability set. That
  # already grants CAP_NET_ADMIN (the TUN interface, used by every tun/udp mode
  # today), CAP_NET_RAW (raw sockets for the dgtun icmp/gre/ipip/ipx encaps) and
  # CAP_SYS_ADMIN (hs2's RAM/CPU-aware sysctl tuning at startup). Adding a
  # restrictive CapabilityBoundingSet=CAP_NET_RAW CAP_NET_ADMIN would DROP
  # CAP_SYS_ADMIN and break that tuning, so no capability directives are added:
  # the raw encaps already have the permissions they need.
  cat > "$SVC" <<EOF
[Unit]
Description=hs2 DPI-resistant tunnel ($role)
Documentation=https://github.com/aliyaghoobi2323-cloud/hs2-
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStartPre=-/sbin/modprobe tun
ExecStart=$BIN run -c $CFG
# reload = hot-swap the TLS certificate (SIGHUP) without dropping the tunnel;
# the certbot renewal deploy-hook calls `systemctl reload hs2`.
ExecReload=/bin/kill -HUP \$MAINPID
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
  unit_unmask hs2
  systemctl enable hs2 >/dev/null 2>&1 || true
  if restart_unit hs2; then
    ok "hs2 ($role) is running with the new config."
    ok "Autostart on boot: ON — it comes back by itself after a reboot or crash."
    info "Manage it any time with:  hs2-menu   → 3) Tunnel manager"
  else
    err "hs2 did not start with the new config. Last log:"
    journalctl -u hs2 -n 20 --no-pager >&2 || true
    bail
  fi
  # The side that pasted the link starts second: the other server is already
  # waiting, so the tunnel must connect NOW. A running service is not proof —
  # a raw encapsulation the path filters (gre/ipip often are) runs happily and
  # carries nothing — so nothing says "ready" until packets really cross.
  if [ "${VERIFY_PEER:-0}" = 1 ] && ! verify_tunnel "$CFG" "${HS2_VERIFY_SECS:-40}"; then
    tunnel_down_help "$CFG"
    bail
  fi
}

# tunnel_up CFG: 0 when the tunnel really carries packets to the other server.
# Every carrier counts a link only after the peer answered its authenticated
# handshake, so a live link in the daemon's status file is proof; the carriers
# without one (udp/auto) are proven by the peer's tun IP answering ping.
tunnel_up(){ # cfg
  local cfg="$1" sf links ifc peer
  sf=$(status_path "$cfg")
  if status_fresh "$sf"; then
    links=$(jraw "$sf" links)
    [ "${links:-0}" -gt 0 ] 2>/dev/null && return 0
  fi
  ifc=$(jget "$cfg" iface); peer=$(jget "$cfg" peer_ip)
  [ -n "$ifc" ] && [ -n "$peer" ] && ip link show "$ifc" >/dev/null 2>&1 \
    && ping -c1 -W1 -I "$ifc" "$peer" >/dev/null 2>&1
}

# verify_tunnel CFG SECS: wait up to SECS for tunnel_up.
verify_tunnel(){ # cfg secs
  local end=$(( $(date +%s) + ${2:-40} ))
  info "Checking that the tunnel really reaches the other server (up to ${2:-40} s)…"
  while :; do
    if tunnel_up "$1"; then ok "Tunnel is UP — the other server answered through it."; return 0; fi
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 2
  done
}

# tunnel_down_help CFG: the tunnel did not connect — say so plainly, with the
# likely cause for this transport, and leave the service running (it retries on
# its own, so a path that opens later still connects without a re-install).
tunnel_down_help(){ # cfg
  local car enc proto
  car=$(jget "$1" carrier); enc=$(jget "$1" encap); proto=$(jraw "$1" proto)
  echo >&2; hr
  err "The tunnel did NOT connect: hs2 is running here, but nothing came back from the other server."
  if [ "$car" = dgtun ]; then
    case "${enc:-udp}" in
      gre)  warn "GRE (IP protocol 47) is dropped by many providers and at the Iran border — the most likely cause." ;;
      ipip) warn "IP-in-IP (IP protocol 4) is dropped by many providers and at the Iran border — the most likely cause." ;;
      ipx)  warn "Raw IP protocol ${proto:-253} is probably filtered on this path (most networks pass only TCP/UDP/ICMP)." ;;
      icmp) warn "ICMP echo may be filtered or rate-limited on this path (or ping is blocked in a firewall on either server)." ;;
      *)    warn "The UDP tunnel port may be blocked on this path or in a firewall on the other server." ;;
    esac
    case "${enc:-udp}" in
      gre|ipip|ipx) warn "Run setup again on BOTH servers and pick tun → udp, icmp, or tcp (tcp + mtcp is the most robust)." ;;
    esac
  elif [ "$car" = udp ] || [ "$car" = auto ]; then
    warn "The UDP tunnel port may be blocked on this path or in a firewall on the other server."
  else
    warn "TLS: the TCP tunnel port must be reachable from the dialing side, and the listening side's"
    warn "certificate must be valid for the domain in the link (see its log for TLS errors)."
  fi
  warn "Also check: the other server finished its setup and runs; the link you pasted is its CURRENT one"
  warn "(running setup there again makes a NEW key); its tunnel port is open in its firewall / provider panel;"
  warn "and the dial-out IP chosen here is not a filtered one."
  info "hs2 stays installed and keeps retrying — if the path opens later it connects by itself."
  info "Last log:"
  journalctl -u hs2 -n 15 --no-pager -o cat 2>/dev/null | sed 's/^/     /' >&2 || true
  hr
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
       --register-unsafely-without-email --deploy-hook "systemctl reload hs2" >/dev/null 2>&1; then
    ok "Certificate obtained for $domain."
  else
    err "certbot HTTP-01 failed for $domain."
    warn "Usual causes on an Iran server: inbound port 80 is filtered, the A record"
    warn "does not point here, or a firewall blocks it."
    warn "Re-run this setup and choose DNS-01 (no port 80) or 'existing certificate'."
    die  "certificate not obtained."
  fi
  configure_renewal "$domain"
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
       --register-unsafely-without-email --deploy-hook "systemctl reload hs2" </dev/tty >&2; then
    ok "Certificate obtained for $domain via DNS-01."
  else
    err "certbot DNS-01 did not complete for $domain."
    warn "Obtain a certificate another way and re-run choosing 'existing certificate'."
    die  "certificate not obtained."
  fi
  configure_renewal "$domain"
  printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
}

# configure_renewal makes an existing certbot renewal do two things the tunnel
# wants: renew 30 days before expiry (certbot's own default — a week left too
# little room when Let's Encrypt or port 80 is unreachable for a few days), and
# reload hs2 (hot
# cert swap, no dropped connections) instead of restarting it. It edits the
# renewal conf in place and makes sure the twice-daily certbot timer is on.
configure_renewal(){ # domain
  local conf="/etc/letsencrypt/renewal/$1.conf"
  [ -f "$conf" ] || return 0
  # deploy hook -> reload
  if grep -q '^renew_hook' "$conf"; then
    sed -i 's#^renew_hook.*#renew_hook = systemctl reload hs2#' "$conf"
  elif grep -q '^\[renewalparams\]' "$conf"; then
    sed -i '/^\[renewalparams\]/a renew_hook = systemctl reload hs2' "$conf"
  else
    printf '[renewalparams]\nrenew_hook = systemctl reload hs2\n' >> "$conf"
  fi
  # renew 30 days before expiry. certbot only reads this key at the TOP of the
  # file (before [renewalparams]); appended at the end it would be ignored.
  if grep -q '^renew_before_expiry' "$conf"; then
    sed -i 's#^renew_before_expiry.*#renew_before_expiry = 30 days#' "$conf"
  elif grep -q '^\[' "$conf"; then
    sed -i '0,/^\[/s//renew_before_expiry = 30 days\n[/' "$conf"
  else
    printf 'renew_before_expiry = 30 days\n' >> "$conf"
  fi
  systemctl enable --now certbot.timer >/dev/null 2>&1 || true
  ok "Renewal set: 30 days before expiry, hot-reload (no downtime). Timer: certbot.timer."
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
    configure_renewal "$domain"
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
  echo "    4) tun  — L3 IP tunnel; sub-menu picks how it crosses the wire" >&2
  echo "              (udp/icmp/gre/ipip/ipx datagram pool, or tcp/TLS)" >&2
  read -rp "Choose [1]: " TR </dev/tty
  case "${TR:-1}" in
    1) TRANSPORT=auto ;;
    2) TRANSPORT=udp ;;
    3) TRANSPORT=tcp ;;
    4) TRANSPORT=tun; ask_tun_encap ;;
    *) die "invalid transport" ;;
  esac
}

# ask_tun_encap picks HOW the L3 tun (transport "tun") crosses the wire and sets
# TUN_ENCAP. The datagram encaps (udp/icmp/gre/ipip/ipx) use carrier "dgtun" — a
# routed TUN over a pool of datagram carriers, shared-key auth, no cert/domain.
# "tcp" keeps the classic L3-over-multi-link-TLS (carrier "l3mtcp", needs a cert).
ask_tun_encap(){
  echo >&2
  echo "  How should the L3 tunnel cross the wire?" >&2
  echo "    1) udp  — datagram pool over UDP (recommended)" >&2
  echo "    2) icmp — datagram pool inside ping/ICMP (best when only ICMP gets through)" >&2
  echo "    3) gre  — datagram pool as GRE" >&2
  echo "    4) ipip — datagram pool as IP-in-IP" >&2
  echo "    5) ipx  — datagram pool over a raw IP protocol number" >&2
  echo "    6) tcp  — L3 over TLS: the mtcp multi-link pool or one TLS link (needs a domain + certificate)" >&2
  read -rp "Choose [1]: " TE </dev/tty
  TUN_PROTO=""
  case "${TE:-1}" in
    1) TUN_ENCAP=udp ;;
    2) TUN_ENCAP=icmp ;;
    3) TUN_ENCAP=gre ;;
    4) TUN_ENCAP=ipip ;;
    5) TUN_ENCAP=ipx; ask_ipx_proto ;;
    6) TUN_ENCAP=tcp; ask_tun_tls_mode ;;
    *) die "invalid tun encapsulation" ;;
  esac
}

# ask_tun_tls_mode sets CARRIER for a tun carried over TLS (tun -> tcp). Both
# choices run the stream engine with hs0 as a side channel and forward the user
# ports exactly like the tcp transport; they differ only in the link pool:
#   l3mtcp = the mtcp multi-link pool (2..32 TLS links sized by the autopilot)
#   tls    = a single TLS link
# Plain mtcp (no TUN) is transport tcp -> mtcp; under "tun" there is always hs0.
ask_tun_tls_mode(){
  echo >&2
  echo "  TLS mode for the tun:" >&2
  echo "    1) mtcp + tun — the mtcp multi-link pool (2–32 TLS links) + hs0 (recommended, fastest)" >&2
  echo "    2) tls  + tun — one TLS link + hs0 (fewer connections, but far slower where each connection is throttled)" >&2
  read -rp "Choose [1]: " M </dev/tty
  case "${M:-1}" in 1) CARRIER=l3mtcp ;; 2) CARRIER=tls ;; *) die "invalid mode" ;; esac
}

# tun_tls_carrier normalizes the carrier a tun -> tcp link carries: l3mtcp or
# tls; anything else (an old link) is the multi-link pool, as it always was.
tun_tls_carrier(){ case "${CARRIER:-}" in tls) CARRIER=tls ;; *) CARRIER=l3mtcp ;; esac; }

# tun_tls_label names the carrier for messages.
tun_tls_label(){ [ "$CARRIER" = tls ] && echo "one TLS link" || echo "mtcp multi-link pool"; }

# ask_ipx_proto sets TUN_PROTO: the raw IP protocol number the ipx encapsulation
# rides on. It must be the SAME number on both servers. 253 is the default
# (experimental range); 1/4/6/17/47 are taken by ICMP/IPIP/TCP/UDP/GRE.
ask_ipx_proto(){
  read -rp "IPX raw IP protocol number (same on both servers) [253]: " IPXP </dev/tty
  IPXP=${IPXP:-253}
  case "$IPXP" in ''|*[!0-9]*) die "protocol number must be a number" ;; esac
  TUN_PROTO="$IPXP"
}

# raw_encap: the tun rides a bare IP protocol (icmp/gre/ipip/ipx) — no ports.
raw_encap(){ [ "${TRANSPORT:-}" = tun ] && case "${TUN_ENCAP:-}" in icmp|gre|ipip|ipx) true ;; *) false ;; esac; }

# raw_encap_wire names what a raw encapsulation needs open on the path.
raw_encap_wire(){
  case "$TUN_ENCAP" in
    icmp) echo "ICMP echo (ping)" ;;
    gre)  echo "IP protocol 47 (GRE)" ;;
    ipip) echo "IP protocol 4 (IP-in-IP)" ;;
    ipx)  echo "IP protocol ${TUN_PROTO:-253}" ;;
  esac
}

# ask_tunnel_port PROMPT sets TPORT and PSUF (":PORT", appended to the listen
# address and to the link's endpoint). The raw encapsulations have no ports, so
# nothing is asked for them: the address is the bare IP and PSUF is empty.
ask_tunnel_port(){ # prompt
  if raw_encap; then
    TPORT=""; PSUF=""
    info "tun over $TUN_ENCAP is a bare IP protocol: no tunnel port. The path must pass $(raw_encap_wire)."
    return 0
  fi
  read -rp "$1 [2096]: " TPORT </dev/tty; TPORT=${TPORT:-2096}
  case "$TPORT" in *[!0-9]*) die "the tunnel port must be a number" ;; esac
  [ "$TPORT" -ge 1 ] && [ "$TPORT" -le 65535 ] || die "the tunnel port must be 1-65535"
  PSUF=":$TPORT"
}

# dgtun_proto_line ENCAP prints the optional  "proto": N  config line — only for
# the ipx encapsulation and only when a non-default protocol number was chosen
# (253/0 = default, left out of the config). Empty for every other case.
dgtun_proto_line(){
  if [ "$1" = ipx ] && [ -n "$TUN_PROTO" ] && [ "$TUN_PROTO" != 253 ] && [ "$TUN_PROTO" != 0 ]; then
    printf '\n  "proto": %s,' "$TUN_PROTO"
  fi
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

show_link(){ # endpoint domain shared panel carrier udp transport direction [mtu] [encap] [proto]
  local L; L=$(encode_link "$1|$2|$3|$4|$5|$6|$7|$8|${9:-}|${10:-}|${11:-}")
  echo >&2; hr
  ok "SETUP LINK — copy it to the OTHER server:"
  _c '1;33' "hs2://$L"
  hr
}

# parse_link reads a pasted hs2:// link into ENDPOINT DOMAIN SHARED PANEL
# CARRIER UDP TRANSPORT DIRECTION MTU ENCAP PROTO (with sensible defaults for
# older links; the trailing MTU/ENCAP/PROTO fields are optional and only used by
# tun mode — PROTO is the ipx protocol number, so the other side never asks it).
parse_link(){
  read -rp "Paste the hs2:// setup link from the OTHER server: " RAW </dev/tty
  RAW=${RAW#hs2://}
  local DEC; DEC=$(decode_link "$RAW") || die "invalid link"
  IFS='|' read -r ENDPOINT DOMAIN SHARED PANEL CARRIER UDP TRANSPORT DIRECTION MTU ENCAP PROTO <<< "$DEC"
  [ -n "$ENDPOINT" ] && [ -n "$SHARED" ] || die "link is missing fields"
  CARRIER=${CARRIER:-mtcp}; UDP=${UDP:-false}
  if [ -z "$TRANSPORT" ]; then
    case "$CARRIER" in udp) TRANSPORT=udp ;; auto) TRANSPORT=auto ;; *) TRANSPORT=tcp ;; esac
  fi
  DIRECTION=${DIRECTION:-direct}
  # An OLD 9-field tun link had no ENCAP and was always the classic l3mtcp path,
  # so default a tun link's ENCAP to "tcp" to keep those links working.
  [ -z "${ENCAP:-}" ] && [ "$TRANSPORT" = "tun" ] && ENCAP=tcp
  ENCAP=${ENCAP:-}
  case "${PROTO:-}" in ''|*[!0-9]*) PROTO="" ;; esac
}

# ipx_proto_from_link sets TUN_PROTO for the ipx encapsulation from the link:
# the side that made the link already chose it, and it must be the same number on
# both servers. Only an older link without the number makes this side ask.
ipx_proto_from_link(){
  if [ -n "${PROTO:-}" ]; then
    TUN_PROTO="$PROTO"; ok "IPX protocol number from the link: $PROTO."
  else
    ask_ipx_proto
  fi
}

# ---------- KHAREJ (foreign server, the panel side) --------------------------
setup_kharej(){
  hr; info "KHAREJ setup (foreign server — the panel side)"; hr
  auto_backup
  install_prereqs
  install_binary
  ask_direction
  if [ "$DIRECTION" = "direct" ]; then
    echo >&2; info "This server's IP addresses:"; show_ips
    echo "   (Public IP = the address the OTHER server connects to; it goes into the link.)" >&2
    local defip; defip=$(first_public_ip)
    read -rp "Public IP of THIS kharej server [$defip]: " PUBIP </dev/tty; PUBIP=${PUBIP:-$defip}
    ip_is_local "$PUBIP" || info "$PUBIP is not on a local interface — treating it as a NAT/public IP of this server (the other side will connect to it)."
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
  ask_tunnel_port "Tunnel port (clients never see this)"
  ask_bind_ip
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
  "addr": "$BINDADDR:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "expose": "$PANEL"
}
EOF
  elif [ "$TRANSPORT" = "tun" ] && [ "$TUN_ENCAP" = "tcp" ]; then
    # L3 tunnel over multi-link TLS (l3mtcp). Kharej is the TLS server here, so
    # the cert lives on kharej (like direct tcp). The TUN is a routed side
    # channel; the panel inbound is forwarded exactly like the tcp transport:
    # the iran edge opens the user ports and every connection rides a stream to
    # the panel set here (expose).
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    read -rp "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    read -rp "Domain (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required"
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; tun_tls_carrier
    read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
    case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": $TUNMTU,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "expose": "$PANEL"
}
EOF
  elif [ "$TRANSPORT" = "tun" ]; then
    # Datagram tun (carrier "dgtun"): a routed TUN over a POOL of datagram
    # carriers ($TUN_ENCAP). Shared-key auth only — NO cert, NO domain. Kharej is
    # the exit/panel side: every user port the iran edge opens arrives on the
    # tunnel's forwarder port and is handed to the panel (expose). The user ports
    # themselves are asked once, on iran; the ipx number travels in the link.
    [ "$TUN_ENCAP" != "udp" ] || udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    ask_tun_params
    read -rp "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    LMTU=1280; CARRIER=dgtun; DOMAIN="-"; UDP=false
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$TUN_ENCAP")
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "dgtun", "encap": "$TUN_ENCAP", "reverse": false,
  "addr": "$BINDADDR$PSUF",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED",${PROTOLINE}
  "expose": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER
}
EOF
  else
    udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    [ "$TRANSPORT" = "auto" ] && { port_free "$TPORT" || die "auto also needs TCP port $TPORT free — pick another."; }
    CARRIER=$(transport_to_carrier "$TRANSPORT"); DOMAIN="-"; UDP=false
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "$BINDADDR:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED"
}
EOF
  fi
  chmod 600 "$CFG"; write_service kharej; start_service kharej
  ok "KHAREJ side is running (direct, transport: $TRANSPORT) and waits for the Iran server."
  info "The tunnel is tested end-to-end when you paste the link on the Iran server."
  local ENCAP_ARG="" PROTO_ARG=""; [ "$TRANSPORT" = "tun" ] && ENCAP_ARG="$TUN_ENCAP"
  [ "$ENCAP_ARG" = ipx ] && PROTO_ARG="${TUN_PROTO:-253}"
  show_link "$PUBIP$PSUF" "$DOMAIN" "$SHARED" "$PANEL" "$CARRIER" "$UDP" "$TRANSPORT" "direct" "$LMTU" "$ENCAP_ARG" "$PROTO_ARG"
  if [ "$TRANSPORT" = "tun" ] && [ "$TUN_ENCAP" = "tcp" ]; then
    info "L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (MTU $LMTU)."
    info "Panel $PANEL receives the user ports you open on the iran side (asked there)."
  elif [ "$TRANSPORT" = "tun" ]; then
    info "Datagram L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (encap $TUN_ENCAP)."
    info "Panel $PANEL receives the user ports you open on the iran side (asked there)."
  fi
  info "On the Iran server: bash install.sh → 2 (Iran) → direction 'direct' → paste the link."
}

# kharej_dialer: reverse exit. Kharej DIALS the iran edge; it pastes the link
# iran generated (which carries iran's endpoint) and forwards to the panel.
kharej_dialer(){
  VERIFY_PEER=1   # the iran edge is already waiting: start_service proves the tunnel
  parse_link
  [ "$DIRECTION" = "reverse" ] || die "this link is a DIRECT link; for reverse, generate the link on the IRAN side first."
  ask_egress_ip
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
  elif [ "$TRANSPORT" = "tun" ] && [ "$ENCAP" = "tcp" ]; then
    # Reverse tun: kharej DIALS the iran edge (TLS client) and runs the L3 pipe.
    # MTU comes from the link so both sides match. The user ports iran opens
    # ride streams to the panel set here (expose), exactly like reverse tcp.
    [ -n "$MTU" ] || die "this link has no MTU field — regenerate it on the iran edge with the new installer."
    tun_tls_carrier   # the link says mtcp pool (l3mtcp) or one TLS link (tls)
    ask_tun_params
    read -rp "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": true,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": $MTU,
  "shared_key": "$SHARED",
  "expose": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    ok "KHAREJ ready (reverse, tun over TLS: $(tun_tls_label)). It dials in to the Iran edge and forwards to $PANEL."
    info "L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (MTU $MTU)."
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse datagram tun (carrier "dgtun", encap $ENCAP from the link): kharej
    # DIALS the iran edge. No cert/domain. Kharej is the exit/panel side: every
    # user port the iran edge opens arrives on the tunnel's forwarder port and is
    # handed to the panel (expose). The user ports were asked on iran, and the
    # ipx number comes with the link — neither is asked again here.
    [ "$ENCAP" = ipx ] && ipx_proto_from_link
    ask_tun_params
    read -rp "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: " PANEL </dev/tty
    PANEL=${PANEL:-127.0.0.1:8443}
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$ENCAP")
    cat > "$CFG" <<EOF
{
  "mode": "listen", "carrier": "dgtun", "encap": "$ENCAP", "reverse": true,
  "addr": "$ENDPOINT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED",${PROTOLINE}
  "expose": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    ok "KHAREJ ready (reverse, tun / datagram pool over $ENCAP). It dials in to the Iran edge and forwards to $PANEL."
    info "Datagram L3 tunnel on $TUNIF once up: this kharej = 10.77.0.2, iran = 10.77.0.1 (encap $ENCAP)."
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
  auto_backup
  install_prereqs
  install_binary
  ask_direction
  if [ "$DIRECTION" = "direct" ]; then
    iran_dialer          # direct: iran dials out to kharej (pastes the link)
  else
    echo >&2; info "This server's IP addresses:"; show_ips
    echo "   (Public IP = the address the OTHER server connects to; it goes into the link.)" >&2
    local defip; defip=$(first_public_ip)
    read -rp "Public IP of THIS iran server [$defip]: " PUBIP </dev/tty; PUBIP=${PUBIP:-$defip}
    ip_is_local "$PUBIP" || info "$PUBIP is not on a local interface — treating it as a NAT/public IP of this server (the other side will connect to it)."
    [ -n "$PUBIP" ] || die "public IP required"
    ask_transport
    iran_listener        # reverse: iran listens for kharej and generates the link
  fi
}

# iran_dialer: direct edge. Iran dials out to kharej; it pastes kharej's link.
iran_dialer(){
  VERIFY_PEER=1   # the kharej is already waiting: start_service proves the tunnel
  parse_link
  [ "$DIRECTION" = "direct" ] || die "this link is a REVERSE link; for reverse, run KHAREJ setup and paste it there instead."
  ok "Link OK — kharej endpoint $ENDPOINT, transport $TRANSPORT (carrier $CARRIER)."
  ask_egress_ip
  mkdir -p "$(dirname "$CFG")"
  if [ "$TRANSPORT" = "tcp" ]; then
    ask_user_ip
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
  elif [ "$TRANSPORT" = "tun" ] && [ "$ENCAP" = "tcp" ]; then
    # L3 tunnel over TLS (l3mtcp = mtcp pool, or tls = one link). Iran is the
    # TLS client here (validates the kharej's domain as SNI). MTU comes from the link so both
    # sides match; the interface name is a local choice. The user ports opened
    # here ride streams to the kharej panel, exactly like the tcp transport.
    [ -n "$MTU" ] || die "this link has no MTU field — regenerate it on the kharej with the new installer."
    tun_tls_carrier   # the link says mtcp pool (l3mtcp) or one TLS link (tls)
    ask_tun_params
    ask_user_ip
    read -rp "User port(s) to open here, forwarded to the kharej panel, comma-separated (e.g. 8443,443; Enter = none, pure routed tun): " PORTS </dev/tty
    for p in ${PORTS//,/ }; do
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": false, "udp": $UDP,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": $MTU,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL", "user_listen_ip": "$USERIP",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    ok "IRAN ready (direct, tun over TLS: $(tun_tls_label))."
    info "L3 tunnel on $TUNIF once up: this iran = 10.77.0.1, kharej = 10.77.0.2 (MTU $MTU)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel."
    else warn "No user ports: pure routed tun. Users can NOT reach the panel through this server unless you route traffic toward 10.77.0.2 yourself."; fi
  elif [ "$TRANSPORT" = "tun" ]; then
    # Datagram tun (carrier "dgtun", encap $ENCAP from the link): iran is the
    # edge, so it opens the user ports (forward_ports) and rides them over the
    # pool to the kharej exit, which hands them to its panel. No cert, no domain —
    # shared-key auth only. The ipx number comes with the link.
    [ "$ENCAP" = ipx ] && ipx_proto_from_link
    ask_tun_params
    ask_user_ip
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443; Enter = none, pure routed tun): " PORTS </dev/tty
    for p in ${PORTS//,/ }; do
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$ENCAP")
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "dgtun", "encap": "$ENCAP", "reverse": false,
  "addr": "$ENDPOINT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED",${PROTOLINE}
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    ok "IRAN ready (direct, tun / datagram pool over $ENCAP)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel."
    else info "Pure routed L3 tunnel on $TUNIF: this iran = 10.77.0.1, kharej = 10.77.0.2. Route traffic toward 10.77.0.2."; fi
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
    warn "$TRANSPORT is an IP tunnel only: no user port listens here, so users can NOT reach the panel through this server unless you route traffic over hs0 yourself. For a panel inbound choose tcp, or tun (udp or tcp)."
    [ "$TRANSPORT" = "auto" ] && info "auto: if UDP is blocked or too lossy, it falls back to TCP silently."
  fi
  info "Backhaul is untouched. Status/logs any time:  bash install.sh → 4"
}

# iran_listener: reverse edge. Iran listens for the kharej (which dials in) and
# generates the link. For tcp it also opens the user ports; for udp/auto it is a
# TUN IP tunnel on hs0.
iran_listener(){
  ask_tunnel_port "Tunnel port to LISTEN on (kharej dials it)"
  ask_bind_ip
  local SHARED; SHARED=$(openssl rand -hex 32)
  local LMTU=""
  mkdir -p "$(dirname "$CFG")"

  if [ "$TRANSPORT" = "tcp" ]; then
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    ask_user_ip
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443): " PORTS </dev/tty
    [ -n "$PORTS" ] || die "at least one port is required"
    for p in ${PORTS//,/ }; do
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
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
  "addr": "$BINDADDR:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN side is running (reverse, tcp) and waits for the Kharej server to dial in. Users will connect on port(s): $PORTS"
  elif [ "$TRANSPORT" = "tun" ] && [ "$TUN_ENCAP" = "tcp" ]; then
    # Reverse tun: iran LISTENS and is the TLS server, so the cert lives HERE.
    # The kharej dials in. The TUN is a routed side channel; the user ports
    # opened here ride streams to the kharej panel, exactly like reverse tcp.
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    ask_user_ip
    read -rp "User port(s) to open here, forwarded to the kharej panel, comma-separated (e.g. 8443,443; Enter = none, pure routed tun): " PORTS </dev/tty
    for p in ${PORTS//,/ }; do
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    read -rp "Domain for THIS iran server (its A record must point to $PUBIP): " DOMAIN </dev/tty
    [ -n "$DOMAIN" ] || die "domain required (the kharej validates it as the TLS name)"
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; tun_tls_carrier
    UDP=false
    if [ -n "$PORTS" ]; then
      read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
      case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    fi
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true, "udp": $UDP,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": $TUNMTU,
  "backend_addr": "builtin",
  "shared_key": "$SHARED",
  "cert_file": "$CERT", "key_file": "$KEY",
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN side is running (reverse, tun over TLS: $(tun_tls_label)) and waits for the Kharej server to dial in."
    info "L3 tunnel on $TUNIF once up: this iran = 10.77.0.1, kharej = 10.77.0.2 (MTU $LMTU)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel (asked on the kharej)."
    else warn "No user ports: pure routed tun. Users can NOT reach the panel through this server unless you route traffic toward 10.77.0.2 yourself."; fi
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse datagram tun (carrier "dgtun", encap $TUN_ENCAP): iran LISTENS
    # (kharej dials in). No cert, no domain — shared-key auth only. Iran is the
    # edge, so it opens the user ports (forward_ports) and rides them over the pool.
    [ "$TUN_ENCAP" != "udp" ] || udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    ask_tun_params
    ask_user_ip
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443; Enter = none, pure routed tun): " PORTS </dev/tty
    for p in ${PORTS//,/ }; do
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    LMTU=1280; CARRIER=dgtun; DOMAIN="-"; UDP=false
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$TUN_ENCAP")
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "dgtun", "encap": "$TUN_ENCAP", "reverse": true,
  "addr": "$BINDADDR$PSUF",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED",${PROTOLINE}
  "forward_ports": "$PORTS", "user_listen_ip": "$USERIP",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN side is running (reverse, tun / datagram pool over $TUN_ENCAP) and waits for the Kharej server to dial in."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel (asked on the kharej)."
    else info "Pure routed L3 tunnel on $TUNIF: this iran = 10.77.0.1, kharej = 10.77.0.2."; fi
  else
    udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    [ "$TRANSPORT" = "auto" ] && { port_free "$TPORT" || die "auto also needs TCP port $TPORT free — pick another."; }
    CARRIER=$(transport_to_carrier "$TRANSPORT"); DOMAIN="-"; UDP=false
    cat > "$CFG" <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true,
  "addr": "$BINDADDR:$TPORT",
  "iface": "hs0", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN side is running (reverse, $TRANSPORT / UDP+FEC) and waits for the Kharej server to dial in."
    info "An IP tunnel is up on hs0 (iran 10.77.0.1, kharej 10.77.0.2)."
    warn "$TRANSPORT is an IP tunnel only: no user port listens here, so users can NOT reach the panel through this server unless you route traffic over hs0 yourself. For a panel inbound choose tcp, or tun (udp or tcp)."
  fi
  # panel is set on the kharej side; leave it blank in the link. The ipx number
  # goes in the link so the kharej uses the same one without asking.
  local ENCAP_ARG="" PROTO_ARG=""; [ "$TRANSPORT" = "tun" ] && ENCAP_ARG="$TUN_ENCAP"
  [ "$ENCAP_ARG" = ipx ] && PROTO_ARG="${TUN_PROTO:-253}"
  show_link "$PUBIP$PSUF" "$DOMAIN" "$SHARED" "-" "$CARRIER" "$UDP" "$TRANSPORT" "reverse" "$LMTU" "$ENCAP_ARG" "$PROTO_ARG"
  info "On the Kharej server: bash install.sh → 1 (Kharej) → direction 'reverse' → paste the link."
  info "The tunnel is tested end-to-end there: that side only says ready once packets really cross."
}

# ---------- uninstall & status ----------------------------------------------
uninstall(){
  local a
  read -rp "Remove the hs2 tunnel from this server? (a backup is saved first) [y/N]: " a </dev/tty || a=n
  case "$a" in y|Y|yes) ;; *) info "Nothing removed."; return 0 ;; esac
  info "Removing hs2…"
  auto_backup
  local pid; pid=$(tm_prop hs2 MainPID)
  systemctl disable --now hs2 2>/dev/null || true
  sleep 1
  # Stop a straggler of THIS tunnel only — its unit's last PID and any process
  # running this config. Never `pkill -x hs2`: other tunnels on the server
  # (hs2-<name>.service) run the same binary and must keep running.
  kill_this_tunnel TERM "$pid"; sleep 1; kill_this_tunnel KILL "$pid"
  # Delete the tunnel interface. Usually hs0, but tun mode may have renamed it, so
  # also read the name from the config before deleting it.
  local IFACE=""
  [ -f "$CFG" ] && IFACE=$(cfg_field iface)
  [ -n "$IFACE" ] && ip link del "$IFACE" 2>/dev/null || true
  ip link del hs0 2>/dev/null || true
  rm -f "$SVC" "$CFG" "$CFG.prev" /etc/sysctl.d/99-hs2.conf /etc/modules-load.d/hs2.conf; systemctl daemon-reload
  ok "hs2 removed (service stopped, hs0 deleted). Backhaul untouched."
  if ls "$BACKUP_DIR"/hs2-*.tar.gz >/dev/null 2>&1; then
    warn "Backups are kept in $BACKUP_DIR (readable by root only). They contain the tunnel key —"
    warn "delete that folder if this server is being handed over or the tunnel is gone for good."
  fi
}

# kill_this_tunnel SIGNAL [PID]: signal this tunnel's hs2 process — PID when it
# is still an hs2 process, and any `hs2 run -c $CFG` — and nothing else.
kill_this_tunnel(){ # signal [pid]
  local sig="$1" pid="${2:-}" p args
  if [ -n "$pid" ] && [ "$pid" != 0 ] && [ -r "/proc/$pid/cmdline" ]; then
    args=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)
    case "$args" in *hs2*) kill -"$sig" "$pid" 2>/dev/null || true ;; esac
  fi
  for p in $(pgrep -x hs2 2>/dev/null || true); do
    args=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null || true)
    case "$args" in *" -c $CFG "*) kill -"$sig" "$p" 2>/dev/null || true ;; esac
  done
  return 0
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

# ---------- tunnel manager ---------------------------------------------------
# Lists every hs2 tunnel on this server with its live state and lets you
# start / stop / restart it, edit its config (validated, applied, rolled back
# if it fails) and follow its log. Tunnels are found from their systemd units,
# so this works for hs2.service today and hs2-<name>.service later.
MENU_BIN=/usr/local/bin/hs2-menu

# Colours for the manager screens (stderr, like the rest of the UI).
C_G=$'\033[1;32m'; C_R=$'\033[1;31m'; C_Y=$'\033[1;33m'; C_B=$'\033[1;34m'; C_D=$'\033[2m'; C_0=$'\033[0m'
say(){ printf '%s\n' "$*" >&2; }
pause(){ read -rp "Press Enter to continue… " _ </dev/tty || true; }

# jget FILE KEY -> string value; jraw FILE KEY -> bare value (true/false/number)
jget(){ { grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$1" 2>/dev/null || true; } | head -1 | sed 's/.*"\([^"]*\)"$/\1/'; }
jraw(){ { grep -o "\"$2\"[[:space:]]*:[[:space:]]*[a-z0-9.]*" "$1" 2>/dev/null || true; } | head -1 | sed 's/.*:[[:space:]]*//'; }

# The daemon publishes a live status file (see cmd/hs2/status.go). status_path
# derives it from a config path exactly as the binary does: /run/hs2/ + the
# absolute config path with '/'->'-' and ' '->'_', + .status.json.
status_path(){
  local p="$1"
  case "$p" in /*) ;; *) p="$PWD/$p" ;; esac
  p=${p#/}; p=${p//\//-}; p=${p// /_}
  echo "/run/hs2/$p.status.json"
}
# status_fresh FILE -> 0 if the file exists and was updated in the last ~7s.
status_fresh(){
  local f="$1" upd now
  [ -f "$f" ] || return 1
  upd=$(jraw "$f" updated); [ -n "$upd" ] || return 1
  now=$(date +%s)
  [ $((now - upd)) -le 7 ]
}

tm_units(){
  local f
  for f in /etc/systemd/system/hs2.service /etc/systemd/system/hs2-*.service; do
    [ -f "$f" ] && basename "$f" .service
  done
  return 0
}
tm_cfg(){ { sed -n 's/^ExecStart=.* -c \([^ ]*\).*/\1/p' "/etc/systemd/system/$1.service" 2>/dev/null || true; } | head -1; }
tm_prop(){ systemctl show -p "$2" --value "$1" 2>/dev/null || true; }

# tm_state UNIT -> running | starting | failing | failed | stopped
tm_state(){
  local a; a=$(tm_prop "$1" ActiveState)
  case "$a" in
    active) echo running ;;
    activating)
      if [ "$(tm_prop "$1" NRestarts)" -gt 0 ] 2>/dev/null; then echo failing; else echo starting; fi ;;
    failed) echo failed ;;
    *) echo stopped ;;
  esac
}
tm_state_label(){
  case "$1" in
    running)  printf '%s● running%s' "$C_G" "$C_0" ;;
    starting) printf '%s◐ starting%s' "$C_Y" "$C_0" ;;
    failing)  printf '%s✗ crashing (restarting every 3s — see the log)%s' "$C_R" "$C_0" ;;
    failed)   printf '%s✗ failed%s' "$C_R" "$C_0" ;;
    *)        printf '%s○ stopped%s' "$C_Y" "$C_0" ;;
  esac
}
tm_uptime(){
  local t s d
  t=$(tm_prop "$1" ActiveEnterTimestamp); [ -n "$t" ] || return 0
  s=$(date -d "$t" +%s 2>/dev/null) || return 0
  d=$(( $(date +%s) - s ))
  if   [ $d -ge 86400 ]; then printf 'up %dd %dh' $((d/86400)) $((d%86400/3600))
  elif [ $d -ge 3600 ];  then printf 'up %dh %dm' $((d/3600)) $((d%3600/60))
  elif [ $d -ge 60 ];    then printf 'up %dm' $((d/60))
  else printf 'up %ds' $d; fi
}
tm_autostart(){ [ "$(systemctl is-enabled "$1" 2>/dev/null || true)" = enabled ]; }

# Does this side open the tunnel connections? mode=dial is the Iran side,
# reverse flips who connects (same rule as the engine).
tm_dials(){
  local mode rev; mode=$(jget "$1" mode); rev=$(jraw "$1" reverse)
  if [ "$mode" = dial ]; then [ "$rev" != true ]; else [ "$rev" = true ]; fi
}
# Live TLS links of a TCP tunnel. Prefer the daemon's own live count (it knows
# exactly how many links are up, and its dynamic target); fall back to counting
# established sockets when no fresh status file is there (older binary).
tm_links(){
  local cfg="$1" car addr host port sf
  car=$(jget "$cfg" carrier)
  case "$car" in mtcp|l3mtcp|l3|tls) ;; *) return 0 ;; esac
  sf=$(status_path "$cfg")
  if status_fresh "$sf"; then
    jraw "$sf" links; return 0
  fi
  addr=$(jget "$cfg" addr); host=${addr%:*}; port=${addr##*:}
  if tm_dials "$cfg"; then
    ss -Htn state established "( dport = :$port and dst $host )" 2>/dev/null | wc -l
  else
    ss -Htn state established "( sport = :$port )" 2>/dev/null | wc -l
  fi
}

# tm_pattern CFG -> a live one-line description of the adaptive parallel-link
# pattern from the status file, e.g. "7 links (5 serving) (shrinking, 2–32) ·
# 251 connections, 18 active · 6.1 Mbit/s · 1 link at its limit". Empty when
# there is no fresh status (older binary or not up).
tm_pattern(){
  local sf; sf=$(status_path "$1")
  status_fresh "$sf" || return 0
  local links target min max users mbit phase sat serving flowing pressed out
  links=$(jraw "$sf" links); target=$(jraw "$sf" target)
  min=$(jraw "$sf" min); max=$(jraw "$sf" max)
  users=$(jraw "$sf" users); mbit=$(jraw "$sf" mbit)
  phase=$(jget "$sf" phase); sat=$(jraw "$sf" sat)
  serving=$(jraw "$sf" serving); flowing=$(jraw "$sf" flowing); pressed=$(jraw "$sf" pressed)
  # A newer daemon (it writes "serving") omits zero counts: absent = 0.
  [ -n "$serving" ] && flowing=${flowing:-0}
  if [ -n "$serving" ]; then
    # Newer daemon: the target counts serving links; retiring ones close
    # by themselves once their connections end.
    out="${links} links"
    [ "$serving" != "$links" ] && out="$out (${serving} serving)"
    [ -n "$target" ] && [ "$target" != "$serving" ] && [ "$target" != 0 ] && out="$out → ${target}"
  elif [ -n "$target" ] && [ "$target" != "$links" ] && [ "$target" != 0 ]; then
    out="${links}→${target} links"
  else
    out="${links} links"
  fi
  if [ -n "$max" ] && [ "$max" != 0 ]; then
    out="$out (${phase:-steady}, ${min}–${max})"
  elif [ -n "$phase" ]; then
    out="$out (${phase})"
  fi
  if [ -n "$users" ] && [ "$users" != 0 ]; then
    if [ -n "$flowing" ]; then out="$out · ${users} connections, ${flowing} active"
    else out="$out · ${users} users"; fi
  fi
  [ -n "$mbit" ] && [ "$mbit" != 0 ] && out="$out · ${mbit} Mbit/s"
  if [ -n "$pressed" ] && [ "$pressed" != 0 ]; then
    if [ "$pressed" = 1 ]; then out="$out · 1 link at its limit"; else out="$out · ${pressed} links at their limit"; fi
  elif [ -z "$serving" ] && [ "$sat" = true ]; then
    out="$out · saturated"
  fi
  echo "$out"
}
# Peers connected to a listening tunnel (who is actually on the other end).
tm_peers(){
  local port; port=$(jget "$1" addr)
  case "$port" in *:*) port=${port##*:} ;; *) return 0 ;; esac   # raw encap: no port, no TCP peers
  ss -Htn state established "( sport = :$port )" 2>/dev/null | awk '{print $4}' | sed 's/:[0-9]*$//' | sort -u | tr '\n' ' '
}
tm_transport(){
  case "$(jget "$1" carrier)" in
    mtcp) echo "tcp (mtcp)" ;; tls) echo "tun over TLS (one link)" ;; l3mtcp|l3) echo "tun over TLS (mtcp pool)" ;;
    dgtun) local e; e=$(jget "$1" encap); echo "tun (datagram pool over ${e:-udp})" ;;
    udp) echo "udp" ;; auto) echo "auto (udp, tcp fallback)" ;; reality) echo "reality" ;; *) echo "tcp (noise)" ;;
  esac
}
tm_role(){ [ "$(jget "$1" mode)" = dial ] && echo "Iran side" || echo "Kharej side"; }
tm_dir(){ [ "$(jraw "$1" reverse)" = true ] && echo reverse || echo direct; }

# tm_healthy UNIT: running and not crash-restarting (same PID over ~5 s).
tm_healthy(){
  local p1 p2
  sleep 1.5; p1=$(tm_prop "$1" MainPID)
  sleep 4;   p2=$(tm_prop "$1" MainPID)
  [ "$(systemctl is-active "$1" 2>/dev/null || true)" = active ] && [ "${p1:-0}" != 0 ] && [ "$p1" = "$p2" ]
}
tm_log_since(){ # UNIT SINCE
  journalctl -u "$1" --since "$2" --no-pager -o cat 2>/dev/null | tail -n 12 | sed 's/^/     /' >&2 || true
}

tm_list(){
  local units=() u cfg st i=0 links extra
  mapfile -t units < <(tm_units)
  echo >&2; hr; say " ${C_B}Tunnel manager${C_0}   ${C_D}($(hostname))${C_0}"; hr
  if [ ${#units[@]} -eq 0 ]; then
    warn "No hs2 tunnel on this server yet."
    info "Create one from the main menu: 1) Kharej or 2) Iran."
    TM_UNITS=(); return 0
  fi
  for u in "${units[@]}"; do
    i=$((i+1)); cfg=$(tm_cfg "$u"); st=$(tm_state "$u")
    extra=""
    [ "$st" = running ] && extra=" · $(tm_uptime "$u")"
    links=$(tm_links "$cfg"); [ "$st" = running ] && [ -n "$links" ] && extra="$extra · $links links"
    if tm_autostart "$u"; then extra="$extra · autostart ON"; else extra="$extra · ${C_Y}autostart OFF${C_0}"; fi
    say "  $i) ${C_B}$u${C_0}  $(tm_state_label "$st")$extra"
    if [ -f "$cfg" ]; then
      say "      $(tm_role "$cfg") · $(tm_dir "$cfg") · $(tm_transport "$cfg") · $(tm_endpoint "$cfg")"
    else
      say "      ${C_R}config file missing: ${cfg:-?}${C_0}"
    fi
  done
  TM_UNITS=("${units[@]}")
}
tm_endpoint(){
  local addr; addr=$(jget "$1" addr)
  if tm_dials "$1"; then echo "connects to $addr"; else echo "listens on $addr"; fi
}

tm_details(){
  local u="$1" cfg="$2" st links bind
  st=$(tm_state "$u"); links=$(tm_links "$cfg")
  echo >&2; hr
  say " Tunnel:      ${C_B}$u${C_0}   ${C_D}config: $cfg${C_0}"
  say " Status:      $(tm_state_label "$st")$([ "$st" = running ] && echo " · $(tm_uptime "$u")")$([ "$st" = running ] && [ -n "$links" ] && echo " · $links links")"
  if [ -f "$cfg" ]; then
    say " Side:        $(tm_role "$cfg") · $(tm_dir "$cfg") · $(tm_transport "$cfg")"
    if tm_dials "$cfg"; then
      bind=$(jget "$cfg" bind_local_ip)
      say " Connection:  connects to $(jget "$cfg" addr) from ${bind:-the default IP}"
    else
      say " Connection:  listens on $(jget "$cfg" addr)$([ "$st" = running ] && [ -n "$(tm_peers "$cfg")" ] && echo " · connected: $(tm_peers "$cfg")")"
    fi
    [ -n "$(jget "$cfg" forward_ports)" ] && say " User ports:  $(jget "$cfg" forward_ports)  (users connect here)"
    [ -n "$(jget "$cfg" expose)" ] && say " Panel:       $(jget "$cfg" expose)"
  fi
  if [ "$st" = running ]; then
    local pat sf cd
    pat=$(tm_pattern "$cfg")
    [ -n "$pat" ] && say " Pattern:     $pat"
    sf=$(status_path "$cfg")
    if status_fresh "$sf"; then
      cd=$(jraw "$sf" cert_days)
      if [ -n "$cd" ] && [ "$cd" != -1 ]; then
        if [ "$cd" -le 7 ] 2>/dev/null; then say " Certificate: ${C_Y}$cd day(s) left${C_0} — auto-renews (hot reload, no downtime)"
        else say " Certificate: valid for $cd more day(s)"; fi
      fi
    fi
  fi
  if tm_autostart "$u"; then say " Autostart:   ${C_G}ON${C_0} — comes back by itself after a reboot"
  else say " Autostart:   ${C_Y}OFF${C_0} — will NOT start after a reboot"; fi
  hr
}

# tm_monitor shows the adaptive link pattern live, refreshing every 2s until the
# user presses a key. This is where the connection pattern is watched changing.
tm_monitor(){
  local u="$1" cfg="$2" st pat sf
  sf=$(status_path "$cfg")
  info "Live pattern of $u — press Enter to go back."
  # Read one key with a 2s timeout as the refresh clock; Enter (or any key) exits.
  while :; do
    st=$(tm_state "$u")
    printf '\033[2J\033[H' >&2
    say " ${C_B}$u${C_0}  $(tm_state_label "$st")$([ "$st" = running ] && echo " · $(tm_uptime "$u")")"
    say " $(tm_role "$cfg") · $(tm_dir "$cfg") · $(tm_transport "$cfg") · $(tm_endpoint "$cfg")"
    hr
    if [ "$st" != running ]; then
      say " (not running)"
    elif status_fresh "$sf"; then
      local links target min max users mbit phase sat serving retiring held heldact flowing pressed capm reason xstats
      links=$(jraw "$sf" links); target=$(jraw "$sf" target)
      min=$(jraw "$sf" min); max=$(jraw "$sf" max)
      users=$(jraw "$sf" users); mbit=$(jraw "$sf" mbit)
      phase=$(jget "$sf" phase); sat=$(jraw "$sf" sat)
      serving=$(jraw "$sf" serving); retiring=$(jraw "$sf" retiring)
      held=$(jraw "$sf" held_by); heldact=$(jraw "$sf" held_active)
      flowing=$(jraw "$sf" flowing); pressed=$(jraw "$sf" pressed); capm=$(jraw "$sf" cap_mbit)
      reason=$(jget "$sf" reason); xstats=$(jget "$sf" exit_stats)
      [ -n "$serving" ] && flowing=${flowing:-0}  # newer daemon: absent count = 0
      local bar="" i=0
      # A little gauge inside the min–max envelope: serving links (█),
      # retiring links still up (▓), links wanted but not up yet (▒).
      if [ -n "$max" ] && [ "$max" != 0 ]; then
        local sv=${serving:-$links} rt=${retiring:-0}
        while [ "$i" -lt "$max" ]; do
          if [ "$i" -lt "$sv" ]; then bar="$bar${C_G}█${C_0}"
          elif [ "$i" -lt $((sv+rt)) ]; then bar="$bar${C_D}▓${C_0}"
          elif [ "$i" -lt "${target:-0}" ]; then bar="$bar${C_Y}▒${C_0}"
          else bar="$bar${C_D}·${C_0}"; fi
          i=$((i+1))
        done
        if [ -n "$serving" ]; then
          say " Links:   ${C_B}${links}${C_0} up = ${serving} serving$([ "${retiring:-0}" != 0 ] && echo " + ${retiring} retiring")$([ -n "$target" ] && [ "$target" != "$serving" ] && echo " · target ${target}") · range ${min}–${max}"
        else
          say " Links:   ${C_B}${links}${C_0} up$([ -n "$target" ] && [ "$target" != "$links" ] && echo " → ${target} target") · range ${min}–${max}"
        fi
        say "          [$bar]"
        if [ -n "$pressed" ] && [ "$pressed" != 0 ]; then
          say " Mode:    ${phase:-steady} · ${pressed} link(s) at their limit$([ -n "$capm" ] && echo " (~${capm} Mbit/s each)")"
        else
          say " Mode:    ${phase:-steady}$([ -z "$serving" ] && [ "$sat" = true ] && echo " · saturated (a bigger pattern may help)")"
        fi
        [ -n "$reason" ] && say " Why:     ${reason}"
        [ "${retiring:-0}" != 0 ] && say " Retiring: ${retiring} link(s) take no new connections and close when theirs end$([ -n "$held" ] && echo " (held by ${held} open$([ -n "$heldact" ] && echo ", ${heldact} active"))")"
        [ -n "$xstats" ] && [ "$xstats" != ok ] && say " Exit:    link stats: ${xstats}"
      else
        say " Links:   ${C_B}${links}${C_0} up (${phase:-running})"
      fi
      if [ -n "$users" ]; then
        if [ -n "$flowing" ]; then say " Users:   ${users} open connections, ${flowing} active"
        else say " Users:   ${users} active connections"; fi
      fi
      [ -n "$mbit" ] && [ "$mbit" != 0 ] && say " Speed:   ${mbit} Mbit/s (tunnel goodput)"
      local peers; peers=$(tm_peers "$cfg")
      [ -n "$peers" ] && say " Peer:    $peers"
    else
      say " Waiting for live status… (needs the new binary; older tunnels show links only)"
      local links; links=$(tm_links "$cfg")
      [ -n "$links" ] && say " Links:   ${links} (from open sockets)"
    fi
    hr
    say " ${C_D}refreshing every 2s · press Enter to go back${C_0}"
    read -rp "" -t 2 _ </dev/tty && break || true
  done
  echo >&2
}

tm_start(){
  local u="$1" since
  if [ "$(tm_state "$u")" = running ]; then ok "$u is already running."; return 0; fi
  since=$(date '+%Y-%m-%d %H:%M:%S')
  info "Starting $u…"
  if restart_unit "$u"; then ok "$u is running."; else err "$u did not stay up. Log:"; fi
  tm_log_since "$u" "$since"
}
tm_stop(){
  local u="$1" a
  if [ "$(tm_state "$u")" = stopped ]; then ok "$u is already stopped."; return 0; fi
  read -rp "Stop $u now? Users are disconnected until it is started again. [y/N]: " a </dev/tty
  case "$a" in y|Y|yes) ;; *) info "Not stopped."; return 0 ;; esac
  systemctl stop "$u" 2>/dev/null || true
  ok "$u stopped."
  tm_autostart "$u" && info "Autostart is ON, so it starts again after a reboot. To keep it off, turn autostart OFF."
  return 0
}
tm_restart(){
  local u="$1" since
  since=$(date '+%Y-%m-%d %H:%M:%S')
  info "Restarting $u…"
  if restart_unit "$u"; then ok "$u restarted and running."; else err "$u did not stay up. Log:"; fi
  tm_log_since "$u" "$since"
}
tm_toggle_autostart(){
  local u="$1"
  if tm_autostart "$u"; then
    systemctl disable "$u" >/dev/null 2>&1 || true
    warn "Autostart OFF: $u will NOT start after a reboot (it keeps running now)."
  else
    systemctl enable "$u" >/dev/null 2>&1 || true
    ok "Autostart ON: $u starts by itself after a reboot."
  fi
}
tm_follow(){
  info "Live log of $1 — press Ctrl+C to go back to the menu."
  # A no-op INT handler (not "ignore") lets Ctrl+C stop journalctl but not us.
  trap ':' INT
  journalctl -u "$1" -f -n 30 -o cat --no-pager </dev/null || true
  trap - INT
  echo >&2
}

tm_editor(){
  if command -v nano >/dev/null 2>&1; then echo nano; return 0; fi
  info "Installing nano…"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q nano </dev/null >/dev/null 2>&1 || true
  if command -v nano >/dev/null 2>&1; then echo nano; elif command -v vi >/dev/null 2>&1; then echo vi; fi
}
# tm_validate FILE: full check with `hs2 check` (old binaries: JSON syntax only).
tm_validate(){
  local out rc=0 line
  out=$("$BIN" check -c "$1" 2>&1) || rc=$?
  if printf '%s' "$out" | grep -q "unknown command"; then
    warn "This hs2 binary is too old for the full check (upgrade: option 5). Checking JSON syntax only."
    if command -v python3 >/dev/null 2>&1; then
      out=$(python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" 2>&1) && { ok "JSON syntax OK."; return 0; }
      err "Not valid JSON: $(printf '%s' "$out" | tail -1)"; return 1
    fi
    warn "python3 not found — could not check the file."; return 0
  fi
  while IFS= read -r line; do
    case "$line" in
      ERROR:*) say "  ${C_R}✗ ${line#ERROR: }${C_0}" ;;
      WARN:*)  say "  ${C_Y}! ${line#WARN:  }${C_0}" ;;
      "config OK") say "  ${C_G}✓ config OK${C_0}" ;;
      *) [ -n "$line" ] && say "  $line" ;;
    esac
  done <<<"$out"
  return $rc
}
tm_edit(){
  local u="$1" cfg="$2" ed tmp c a since
  [ -f "$cfg" ] || { err "Config file not found: $cfg"; return 0; }
  ed=$(tm_editor); [ -n "$ed" ] || { err "No text editor available (apt install nano)."; return 0; }
  tmp=$(mktemp /tmp/hs2-edit.XXXXXX); chmod 600 "$tmp"; cp "$cfg" "$tmp"
  echo >&2
  info "The config opens in $ed."
  [ "$ed" = nano ] && info "Save: Ctrl+O then Enter  ·  Close: Ctrl+X"
  info "After you close it, the change is checked and $u restarts automatically."
  info "If the new config fails, you can put the old one back with one key."
  read -rp "Press Enter to open the editor… " _ </dev/tty || true
  while :; do
    "$ed" "$tmp" </dev/tty >/dev/tty 2>&1 || true
    if cmp -s "$tmp" "$cfg"; then
      rm -f "$tmp"; info "No changes — $u was not restarted."; return 0
    fi
    say ""; say " Your changes:"
    diff -u "$cfg" "$tmp" 2>/dev/null | tail -n +3 | grep '^[-+]' | sed -e "s/^-/  ${C_R}- /" -e "s/^+/  ${C_G}+ /" -e "s/\$/${C_0}/" >&2 || true
    say ""; info "Checking the new config…"
    if tm_validate "$tmp"; then break; fi
    say ""; say "  The new config has errors, so it was NOT applied. $u is untouched."
    say "    1) Open the editor again to fix it"
    say "    2) Throw away my changes"
    read -rp "  Choose [1]: " c </dev/tty || c=2
    case "${c:-1}" in 2) rm -f "$tmp"; info "Changes thrown away. Nothing was changed."; return 0 ;; esac
  done
  cp -p "$cfg" "$cfg.prev"
  cat "$tmp" > "$cfg"; rm -f "$tmp"
  ok "Saved. The previous version is kept as $cfg.prev"
  since=$(date '+%Y-%m-%d %H:%M:%S')
  info "Restarting $u to apply the change…"
  systemctl restart "$u" 2>/dev/null || true
  if tm_healthy "$u"; then
    ok "Applied — $u is running with the new config."
    tm_log_since "$u" "$since"
    return 0
  fi
  err "$u did not come up with the new config. Log:"
  tm_log_since "$u" "$since"
  read -rp "Put the previous config back and restart? [Y/n]: " a </dev/tty || a=y
  case "${a:-y}" in
    n|N|no) warn "Left the new config in place. Fix it with Edit, or restore $cfg.prev." ;;
    *)
      cat "$cfg.prev" > "$cfg"
      since=$(date '+%Y-%m-%d %H:%M:%S')
      systemctl restart "$u" 2>/dev/null || true
      if tm_healthy "$u"; then ok "Previous config restored — $u is running again."
      else err "$u is still not running with the previous config. Log:"; tm_log_since "$u" "$since"; fi ;;
  esac
}

# tm_apply_restart restarts a tunnel after a config change and rolls back to
# $cfg.prev if it does not come up — the same safety net as the editor.
tm_apply_restart(){ # unit cfg
  local u="$1" cfg="$2" since
  since=$(date '+%Y-%m-%d %H:%M:%S')
  info "Restarting $u to apply the change…"
  if restart_unit "$u"; then ok "Applied — $u is running with the change."; tm_log_since "$u" "$since"; return 0; fi
  err "$u did not come up with the change. Rolling back."
  if [ -f "$cfg.prev" ]; then cat "$cfg.prev" > "$cfg"; fi
  if restart_unit "$u"; then ok "Rolled back — $u is running again."; else err "$u is still down. Check the log."; tm_log_since "$u" "$since"; fi
  # Always 0: the outcome is reported above, and callers use this as the last
  # command of an && list — a non-zero status there would end the whole menu
  # under set -e.
  return 0
}

# tm_cfgset writes one config key via the binary (JSON-aware, validated). It
# refuses cleanly on an old binary that has no `config` command.
tm_cfgset(){ # cfg key value
  local out; out=$("$BIN" config -c "$1" set "$2" "$3" 2>&1) || { err "$out"; return 1; }
  return 0
}

# tm_tune is the tuning screen: it shows exactly what hs2 will apply on this
# server (so nothing is a mystery) and lets the operator choose auto / manual /
# off and the congestion control and qdisc. Changes are written to the config
# and applied with the rollback safety net.
tm_tune(){
  local u="$1" cfg="$2" c v
  if "$BIN" tune -c "$cfg" 2>&1 | grep -q "unknown command"; then
    warn "This hs2 binary is too old for tuning control. Upgrade first (menu → 5)."; pause; return 0
  fi
  while :; do
    echo >&2; hr; say " ${C_B}Kernel tuning${C_0} — $u"; hr
    "$BIN" tune -c "$cfg" 2>/dev/null | sed 's/^/  /' >&2 || true
    hr
    say "  hs2 sizes kernel tuning from this server's RAM and CPU cores and"
    say "  re-applies it every time the service starts."
    say "  1) Auto (recommended)  — sized automatically from RAM & CPU"
    say "  2) Manual              — auto values plus your own buffer sizes"
    say "  3) Off                 — do not touch system sysctls (you tune it yourself)"
    say "  4) Congestion control  — bbr (default) / cubic / …"
    say "  5) Queue discipline    — fq_codel (default) / fq / cake"
    say "  0) Back"
    read -rp "Choose: " c </dev/tty || return 0
    case "$c" in
      1) cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
         tm_cfgset "$cfg" tuning.mode auto && tm_apply_restart "$u" "$cfg" ;;
      2) tm_tune_manual "$u" "$cfg" ;;
      3) cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
         tm_cfgset "$cfg" tuning.mode off && tm_apply_restart "$u" "$cfg" ;;
      4) read -rp "Congestion control [bbr]: " v </dev/tty; v=${v:-bbr}
         cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
         tm_cfgset "$cfg" tuning.congestion "$v" && tm_apply_restart "$u" "$cfg" ;;
      5) read -rp "Queue discipline [fq_codel]: " v </dev/tty; v=${v:-fq_codel}
         cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
         tm_cfgset "$cfg" tuning.qdisc "$v" && tm_apply_restart "$u" "$cfg" ;;
      0|b|B|"") return 0 ;;
      *) warn "Invalid choice." ;;
    esac
  done
}

# tm_tune_manual offers RAM/CPU-tier presets (examples the operator asked for),
# switches the config to manual mode and applies them.
tm_tune_manual(){ # unit cfg
  local u="$1" cfg="$2" c m r w b s
  echo >&2
  say "  Manual presets (a good starting point for the server's size):"
  say "   1) Low    — ~1 GB RAM / 1 core     · buffers 8 MB,  backlog 2048, somaxconn 1024"
  say "   2) Medium — 2–4 GB RAM             · buffers 16 MB, backlog 8192, somaxconn 4096"
  say "   3) High   — ≥4 GB RAM, ≥4 cores    · buffers 32 MB, backlog 16384, somaxconn 8192"
  say "   4) Custom — enter the send/receive buffer size in MB"
  say "   0) Back"
  read -rp "Choose: " c </dev/tty || return 0
  case "$c" in
    1) r=8388608;  w=8388608;  b=2048;  s=1024 ;;
    2) r=16777216; w=16777216; b=8192;  s=4096 ;;
    3) r=33554432; w=33554432; b=16384; s=8192 ;;
    4) read -rp "Buffer size in MB (e.g. 24): " m </dev/tty
       case "$m" in ''|*[!0-9]*) warn "Not a number."; return 0 ;; esac
       r=$((m*1024*1024)); w=$r; b=8192; s=4096 ;;
    0|b|B|"") return 0 ;;
    *) warn "Invalid choice."; return 0 ;;
  esac
  cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
  tm_cfgset "$cfg" tuning.mode manual || return 0
  tm_cfgset "$cfg" tuning.rmem_max "$r" || return 0
  tm_cfgset "$cfg" tuning.wmem_max "$w" || return 0
  tm_cfgset "$cfg" tuning.netdev_backlog "$b" || return 0
  tm_cfgset "$cfg" tuning.somaxconn "$s" || return 0
  tm_apply_restart "$u" "$cfg"
}

tm_tunnel_menu(){
  local u="$1" cfg c
  cfg=$(tm_cfg "$u")
  while :; do
    tm_details "$u" "$cfg"
    say "  1) Start"
    say "  2) Stop"
    say "  3) Restart"
    say "  4) Edit config (nano) — applied automatically when you close it"
    say "  5) Live log"
    say "  6) Live pattern monitor (parallel links, updating)"
    if tm_autostart "$u"; then say "  7) Turn autostart OFF"; else say "  7) Turn autostart ON"; fi
    say "  8) Tuning (kernel network tuning — auto by RAM/CPU, or manual)"
    say "  0) Back"
    read -rp "Choose: " c </dev/tty || return 0
    case "$c" in
      1) tm_start "$u" ;;
      2) tm_stop "$u" ;;
      3) tm_restart "$u" ;;
      4) tm_edit "$u" "$cfg" ;;
      5) tm_follow "$u" ;;
      6) tm_monitor "$u" "$cfg" ;;
      7) tm_toggle_autostart "$u" ;;
      8) tm_tune "$u" "$cfg" ;;
      0|b|B|"") return 0 ;;
      *) warn "Invalid choice." ;;
    esac
  done
}

tunnel_manager(){
  local c
  while :; do
    tm_list
    [ ${#TM_UNITS[@]} -gt 0 ] || { pause; return 0; }
    say ""; say "  Pick a tunnel number to manage it · r) Refresh · 0) Back"
    read -rp "Choose: " c </dev/tty || return 0
    case "$c" in
      0|b|B|q) return 0 ;;
      r|R|"") continue ;;
      *[!0-9]*) warn "Invalid choice." ;;
      *) if [ "$c" -ge 1 ] && [ "$c" -le ${#TM_UNITS[@]} ]; then tm_tunnel_menu "${TM_UNITS[$((c-1))]}"
         else warn "There is no tunnel $c."; fi ;;
    esac
  done
}

# Keep a copy of this script as the `hs2-menu` command, so the menu works even
# when GitHub is unreachable (Iran side).
install_self(){
  local tmp; tmp=$(mktemp)
  if curl -fsSL --connect-timeout 10 -o "$tmp" "$REPO_RAW/install.sh" 2>/dev/null && grep -q 'hs2 v3' "$tmp"; then :
  elif [ -f "$0" ] && grep -q 'hs2 v3' "$0" 2>/dev/null; then cp "$0" "$tmp"
  else rm -f "$tmp"; return 0; fi
  install -m755 "$tmp" "$MENU_BIN"; rm -f "$tmp"
  return 0
}

# ---------- backup & restore -------------------------------------------------
# A backup is one tar.gz holding everything hs2 installs: config, service unit,
# binary, kernel tuning and the TLS cert the config points to (the whole
# letsencrypt lineage when it is a certbot cert, so renewal keeps working).
BACKUP_DIR=/root/hs2-backups

cfg_field(){ # key -> value of a "key": "value" string field in $CFG
  # Never fails: a missing file or key yields "" (under set -e + pipefail a
  # failing grep here would silently end the whole script, e.g. in restore
  # right after an uninstall).
  { grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$CFG" 2>/dev/null || true; } | head -1 | sed 's/.*"\([^"]*\)"$/\1/'
}

backup(){
  [ -f "$CFG" ] || { warn "Nothing to back up ($CFG missing)."; return 0; }
  mkdir -p "$BACKUP_DIR"; chmod 700 "$BACKUP_DIR"
  local f="$BACKUP_DIR/hs2-$(hostname -s 2>/dev/null || echo host)-$(date +%Y%m%d-%H%M%S).tar.gz"
  local items=("${CFG#/}") p
  for p in "$SVC" "$BIN" /etc/sysctl.d/99-hs2.conf; do [ -e "$p" ] && items+=("${p#/}"); done
  for p in "$(cfg_field cert_file)" "$(cfg_field key_file)"; do
    [ -n "$p" ] || continue
    case "$p" in
      /etc/letsencrypt/live/*)
        local d; d=$(echo "$p" | cut -d/ -f5)
        for q in "/etc/letsencrypt/live/$d" "/etc/letsencrypt/archive/$d" "/etc/letsencrypt/renewal/$d.conf"; do
          [ -e "$q" ] && items+=("${q#/}")
        done ;;
      *) [ -e "$p" ] && items+=("${p#/}") ;;
    esac
  done
  # Dedupe (cert and key share a lineage) and write a human-readable summary.
  mapfile -t items < <(printf '%s\n' "${items[@]}" | sort -u)
  local meta; meta=$(mktemp -d)
  {
    echo "hs2 backup $(date -Is) on $(hostname)"
    "$BIN" version 2>/dev/null || true
    echo "mode=$(cfg_field mode) carrier=$(cfg_field carrier) addr=$(cfg_field addr) iface=$(cfg_field iface) bind_local_ip=$(cfg_field bind_local_ip)"
    grep -o '"reverse"[[:space:]]*:[[:space:]]*[a-z]*' "$CFG" || true
    echo "service: $(systemctl is-active hs2 2>/dev/null)"
  } > "$meta/hs2-backup-info.txt"
  tar -czf "$f" -C / "${items[@]}" -C "$meta" hs2-backup-info.txt || { rm -rf "$meta"; die "backup failed"; }
  rm -rf "$meta"; chmod 600 "$f"
  ok "Backup saved: $f"
  tar -tzf "$f" | sed 's/^/     /' >&2
  prune_backups
  echo "$f"
}

# prune_backups keeps the newest $BACKUP_KEEP backups (HS2_KEEP_BACKUPS, default
# 10). Every setup, upgrade and uninstall saves one and each holds the tunnel
# key, so without a limit they pile up (dozens after a few weeks of testing).
BACKUP_KEEP=${HS2_KEEP_BACKUPS:-10}
prune_backups(){
  local old n=0 f
  case "$BACKUP_KEEP" in ''|*[!0-9]*|0) return 0 ;; esac
  old=$(ls -1t "$BACKUP_DIR"/hs2-*.tar.gz 2>/dev/null | tail -n +$((BACKUP_KEEP + 1)) || true)
  for f in $old; do rm -f "$f" && n=$((n + 1)); done
  [ "$n" = 0 ] || info "Removed $n old backup(s); the newest $BACKUP_KEEP are kept in $BACKUP_DIR."
  return 0
}

restore(){
  local f="${1:-}"
  if [ -z "$f" ]; then
    f=$(ls -1t "$BACKUP_DIR"/hs2-*.tar.gz 2>/dev/null | head -1)
    [ -n "$f" ] || die "no backups in $BACKUP_DIR"
    info "Backups (newest first):"; ls -1t "$BACKUP_DIR"/hs2-*.tar.gz | sed 's/^/   /' >&2
    # read -p prints its prompt on stderr, so stderr must stay visible here.
    local ans=""
    if [ -r /dev/tty ]; then
      read -rp "Restore which file? (Enter = newest) [$f]: " ans </dev/tty || true
      f=${ans:-$f}
    fi
  fi
  [ -f "$f" ] || die "backup not found: $f"
  # No `grep -q` here: it exits at the first match, tar then dies of SIGPIPE
  # while still listing, and pipefail turns that into a false "not a backup".
  tar -tzf "$f" 2>/dev/null | grep -x "${CFG#/}" >/dev/null || die "$f is not an hs2 backup (no ${CFG#/} inside)."
  hr; info "Restoring $f"; tar -xzOf "$f" hs2-backup-info.txt 2>/dev/null | sed 's/^/   /' >&2; hr
  # Take down the running tunnel (and its interface: the restored config may use
  # another name) before files are replaced.
  HS2_STOPPED=1
  systemctl stop hs2 2>/dev/null || true
  local IFACE; IFACE=$(cfg_field iface)
  [ -n "$IFACE" ] && ip link del "$IFACE" 2>/dev/null || true
  unit_unmask hs2   # else the restored unit file lands on a /dev/null symlink
  tar -xzf "$f" -C / --exclude=hs2-backup-info.txt || die "extract failed"
  systemctl daemon-reload
  [ -f /etc/sysctl.d/99-hs2.conf ] && sysctl -p /etc/sysctl.d/99-hs2.conf >/dev/null 2>&1 || true
  systemctl enable hs2 >/dev/null 2>&1 || true
  if restart_unit hs2; then
    ok "Restored and running: $("$BIN" version 2>/dev/null)"
    ok "Autostart on boot: ON"
    info "Watch the log:  journalctl -u hs2 -f"
  else
    err "hs2 failed to start after restore. Last log:"; journalctl -u hs2 -n 20 --no-pager >&2; bail
  fi
}

# migrate_config brings an older install's config and system state up to date on
# upgrade, without disturbing anything the operator has hand-tuned:
#   - the parallel-link pool moves to the new adaptive envelope, but ONLY when it
#     is still exactly an old default line (hand-edited values are left alone);
#   - the old static sysctl file is removed (hs2 now tunes at runtime) and BBR is
#     made available for boot;
#   - existing Let's Encrypt renewals switch to hot-reload + 30-day renewal.
migrate_config(){
  local changed=""
  local olds=(
    '"min_links": 4, "max_links": 16, "per_link": 50,'
    '"min_links": 8, "max_links": 16, "per_link": 8,'
  )
  local new="\"min_links\": $LINK_MIN, \"max_links\": $LINK_MAX, \"per_link\": $LINK_PER,"
  local o
  for o in "${olds[@]}"; do
    if grep -qF "$o" "$CFG"; then
      # sed with | delimiter; the pattern has no | so this is safe.
      sed -i "s|$(printf '%s' "$o" | sed 's/[.[\*^$/]/\\&/g')|$new|" "$CFG"
      changed=1
    fi
  done
  [ -n "$changed" ] && ok "Adaptive link pool updated to $LINK_MIN–$LINK_MAX (auto-sized; was a fixed default)."

  # Old tun configs keep working as they are: the classic L3-over-multi-link-TLS
  # (carrier l3mtcp) and the single-carrier udp/auto TUN are unchanged in the new
  # binary, so an upgrade never rewrites them — a datagram-tun cutover changes the
  # wire protocol and must be done on BOTH servers at once, so it is never forced
  # here. Just let the operator know the new option exists. (Detected, not changed.)
  local car ifc
  car=$(cfg_field carrier); ifc=$(cfg_field iface)
  if [ "$car" = l3mtcp ] || [ "$car" = l3 ]; then
    info "This is a classic tun over multi-link TLS (l3mtcp) — still supported and unchanged."
    info "New: a datagram tun (udp/icmp/gre/ipip/ipx, no TCP-in-TCP) is available. To switch, reconfigure BOTH servers (menu → Iran/Kharej → tun) with the same encapsulation."
  elif { [ "$car" = udp ] || [ "$car" = auto ]; } && [ -n "$ifc" ] && grep -q '"local_cidr"' "$CFG" 2>/dev/null; then
    info "This is a single-carrier $car TUN — still supported and unchanged."
    info "New: the datagram tun (carrier dgtun) runs a self-sizing POOL of $car carriers with the autopilot and optional user-port forwarding. To switch, reconfigure BOTH servers (menu → tun → $car)."
  fi

  # hs2 now owns tuning at runtime — drop the old static file, keep BBR for boot.
  if [ -f /etc/sysctl.d/99-hs2.conf ]; then
    rm -f /etc/sysctl.d/99-hs2.conf
    info "Removed the old /etc/sysctl.d/99-hs2.conf — hs2 now applies RAM/CPU-aware tuning at startup."
  fi
  modprobe tcp_bbr 2>/dev/null || true
  echo tcp_bbr > /etc/modules-load.d/hs2.conf 2>/dev/null || true

  # Point the certbot renewal of the certificate THIS tunnel uses at reload +
  # 30-day window. Other certbot lineages on the box (a panel's own certificate,
  # say) are not ours and are left alone.
  # NB: written with `if`, never `grep … && …` as the last command of a loop or
  # function: under set -e a non-matching last lineage used to end the whole
  # upgrade silently — after the service had been stopped for the new binary.
  local d conf
  for conf in /etc/letsencrypt/renewal/*.conf; do
    [ -f "$conf" ] || continue
    d=$(basename "$conf" .conf)
    if grep -q "/etc/letsencrypt/live/$d/" "$CFG" 2>/dev/null; then
      configure_renewal "$d"
    fi
  done
  return 0
}

# Before anything replaces an existing install, keep a copy to roll back to.
auto_backup(){
  [ -f "$CFG" ] || return 0
  info "Existing hs2 install found — backing it up first (restore: bash install.sh restore)."
  backup >/dev/null
}

# Upgrade in place: new binary + kernel tuning, same config and hs2:// link.
upgrade(){
  [ -f "$CFG" ] || die "hs2 is not installed on this server ($CFG missing). Run without 'upgrade' to install."
  hr; info "Upgrading hs2 (config and link stay the same)"; hr
  auto_backup
  install_prereqs
  install_binary
  migrate_config
  # Old installs have an older unit: rewrite it and make sure it starts on boot.
  local role=kharej; grep -q '"mode"[[:space:]]*:[[:space:]]*"dial"' "$CFG" && role=iran
  write_service "$role"
  systemctl enable hs2 >/dev/null 2>&1 || true
  if restart_unit hs2; then
    ok "Autostart on boot: $(systemctl is-enabled hs2 2>/dev/null)"
    info "Tunnel manager: run  hs2-menu  → 3"
    ok "hs2 upgraded and running. Upgrade the OTHER server too (both sides must match)."
    if ! verify_tunnel "$CFG" "${HS2_VERIFY_SECS:-30}"; then
      warn "The tunnel has not reconnected yet. If the OTHER server still runs the old version, upgrade it"
      warn "too — it reconnects then. If both are upgraded and it stays down: journalctl -u hs2 -n 40 --no-pager"
    fi
    info "Watch the log:  journalctl -u hs2 -f"
  else
    err "hs2 failed to start after upgrade. Last log:"
    journalctl -u hs2 -n 20 --no-pager >&2
    bail
  fi
}

# Non-interactive: bash install.sh upgrade   (or: curl … | bash -s upgrade)
case "${1:-}" in
  manage)  tunnel_manager; exit 0 ;;
  upgrade) upgrade; exit 0 ;;
  backup)  backup >/dev/null; exit 0 ;;
  restore) restore "${2:-}"; exit 0 ;;
esac

# ---------- menu -------------------------------------------------------------
main_menu(){
  local CH
  while :; do
    echo >&2
    _c '1;36' "╔══════════════════════════════════════════╗"
    _c '1;36' "║   hs2 v3 — DPI-resistant tunnel           ║"
    _c '1;36' "║   runs alongside Backhaul                 ║"
    _c '1;36' "╚══════════════════════════════════════════╝"
    echo >&2
    echo "  Set up a tunnel" >&2
    echo "    1) Kharej  (foreign server — panel side)" >&2
    echo "    2) Iran    (opens user ports → panel)" >&2
    echo "  Manage" >&2
    echo "    3) Tunnel manager  (list · start/stop/restart · edit · logs)" >&2
    echo "    4) Status / logs" >&2
    echo "    5) Upgrade (new binary, keep config)" >&2
    echo "    6) Backup current config" >&2
    echo "    7) Restore a backup" >&2
    echo "    8) Uninstall hs2" >&2
    echo "    0) Exit" >&2
    echo >&2
    read -rp "Choose [0-8]: " CH </dev/tty || exit 0
    case "$CH" in
      1) setup_kharej; exit 0 ;;
      2) setup_iran; exit 0 ;;
      3) tunnel_manager ;;
      4) status; pause ;;
      5) upgrade; exit 0 ;;
      6) backup >/dev/null; pause ;;
      7) restore; exit 0 ;;
      8) uninstall; exit 0 ;;
      0|q|Q) exit 0 ;;
      *) warn "Invalid choice: pick a number from the list." ;;
    esac
  done
}
main_menu

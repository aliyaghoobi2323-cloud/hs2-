#!/usr/bin/env bash
# ============================================================================
#  hs2 — DPI-resistant tunnel (stream mode over multi-link TLS)
#  Runs ALONGSIDE Backhaul without touching it.
#      bash install.sh            (menu)
#      bash install.sh upgrade    (new binary; every tunnel restarted on its own config)
#      bash install.sh backup     (save every tunnel's config+unit+cert and the binary)
#      bash install.sh restore [file]  (roll back to a backup; newest by default)
#      bash install.sh manage     (tunnel manager: start/stop/restart/edit/logs/delete)
#  Several tunnels run side by side, each its own service: "hs2" (the default)
#  and "hs2-<name>". The name is chosen where the setup link is made and
#  travels in the link, with the tunnel's own tun subnet.
#      hs2-menu                   (this menu, installed locally by setup/upgrade)
# ============================================================================
set -euo pipefail
# Create every file and directory root-only by default (configs hold the tunnel
# key). World-readable artifacts (the binary, the menu) set their mode
# explicitly with `install -m755`, so this never makes them unreadable; it only
# closes the brief window where a config or /etc/hs2 existed at 0644/0755 before
# its explicit chmod.
umask 077

BIN=/usr/local/bin/hs2
REPO_RAW="${HS2_REPO_RAW:-https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main}"
CFG_DIR=/etc/hs2
UNIT_DIR=/etc/systemd/system

# Every tunnel is its own systemd service, so several run side by side and each
# is started, stopped, edited and deleted on its own. The default tunnel is
# "hs2" (config /etc/hs2/config.json — what every older install has); more are
# "hs2-<name>" (config /etc/hs2/hs2-<name>.json). UNIT/CFG/SVC always describe
# the tunnel being set up or operated on; use_unit switches all three.
UNIT=hs2
CFG=$CFG_DIR/config.json
SVC=$UNIT_DIR/hs2.service

# Each tunnel has its own /30 inside 10.77.0.0/16 for the tun addresses: iran is
# base+1, kharej base+2. It travels in the setup link, so both sides always
# agree; set_tun_subnet fills these in. 10.77.0.0/30 is what older links and
# installs use.
TUN_BASE=10.77.0.0
TUN_SUBNET_IRAN="10.77.0.1/30"
TUN_SUBNET_KHAREJ="10.77.0.2/30"
TUN_PEER_IRAN="10.77.0.2"
TUN_PEER_KHAREJ="10.77.0.1"
TUN_IP_IRAN=10.77.0.1
TUN_IP_KHAREJ=10.77.0.2
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

# Safety net. Restore stops the tunnels before files are replaced and records
# them in HS2_STOPPED_UNITS. If the script ends for ANY reason before they run
# again — an error, an unexpected set -e stop, Ctrl+C at a prompt — each one is
# started again with whatever config is in place, so nothing is left down. An
# exit that was not a deliberate `die` also says exactly which command stopped
# it. (Setting up or upgrading never stops a tunnel up front: the new binary
# replaces the file under a running one, which keeps running until its own
# restart — so an aborted setup leaves every tunnel as it was.)
HS2_STOPPED_UNITS=""
# Units whose config THIS run stashed to <cfg>.pre-replace (set only by
# stop_for_replace). on_exit restores a stash ONLY for a unit in this list, so a
# stale stash orphaned by an earlier hard-crash is never applied over a config
# some other operation (e.g. restore) has since put in place.
HS2_STASHED_UNITS=""
HS2_DIED=0
on_exit(){
  local rc=$? cmd=$BASH_COMMAND u uc
  if [ "$rc" != 0 ] && [ "$HS2_DIED" != 1 ]; then
    err "The installer stopped unexpectedly (status $rc) at: $cmd"
    err "Please send this line to the developer."
  fi
  for u in $HS2_STOPPED_UNITS; do
    [ -f "$UNIT_DIR/$u.service" ] || continue
    [ "$(systemctl is-active "$u" 2>/dev/null || true)" != active ] || continue
    # An interrupted re-setup overwrote this tunnel's config in place; put the
    # stashed previous config back so it restarts on what actually worked, not a
    # half-written new one. Restore ONLY a stash THIS run created (in
    # HS2_STASHED_UNITS) — never a stale one left by an earlier hard-crash, which
    # could otherwise clobber a config another operation (e.g. restore) just
    # wrote. start_service removes the stash once the new config is live.
    case " $HS2_STASHED_UNITS " in
      *" $u "*)
        uc=$(tm_cfg "$u" 2>/dev/null || true); [ -n "$uc" ] || uc=$(unit_cfg "$u")
        if [ -n "$uc" ] && [ -f "$uc.pre-replace" ]; then
          warn "$u: an interrupted re-setup — restoring its previous config before starting it."
          cp -p "$uc.pre-replace" "$uc" 2>/dev/null || true
          rm -f "$uc.pre-replace"
        fi ;;
    esac
    warn "$u was stopped and is not running — starting it again with its current config…"
    systemctl start "$u" 2>/dev/null || true
    sleep 2
    if [ "$(systemctl is-active "$u" 2>/dev/null || true)" = active ]; then ok "$u is running again."
    else err "$u could not be started — see: journalctl -u $u -n 40 --no-pager"; fi
  done
  return 0
}
trap on_exit EXIT

# Sweep any .pre-replace config stash left behind by an earlier run that was
# hard-killed (power loss, OOM, SIGKILL) mid re-setup — the EXIT trap never ran,
# so the stash survived. It is dead now (on_exit only restores a stash THIS run
# created) and holds a copy of the tunnel key, so clear it on every start. A
# stash this run creates is made later, by stop_for_replace, so it is untouched.
for _st in "$CFG_DIR"/*.pre-replace; do [ -e "$_st" ] && rm -f "$_st"; done 2>/dev/null || true

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
# interface, which hid secondary IPs on multi-IP servers) — except the tun
# interfaces of hs2's own tunnels: with one tunnel up, its 10.77.x.y must not be
# offered as this server's public / listen / dial-out IP for the next one.
local_ips(){
  local tunifs ifc a out=""
  tunifs=" $(for u in $(tm_units); do cfg_tun_iface "$(tm_cfg "$u")"; done | tr '\n' ' ') "
  # Build the whole list, then print it once. A per-line `echo` inside the loop
  # died of SIGPIPE the moment any consumer closed the pipe early (`head -1` on
  # a 6-IP server), and under set -euo pipefail that aborted the installer
  # ("stopped unexpectedly at: defip=$(first_public_ip)").
  while read -r ifc a; do
    case "$tunifs" in *" $ifc "*) continue ;; esac
    out="$out${a%%/*}"$'\n'
  done < <(ip -4 -o addr show 2>/dev/null | awk '$2!="lo"{print $2, $4}')
  printf '%s' "$out"
}
show_ips(){ local_ips | sed 's/^/   /' >&2; }
first_public_ip(){ local_ips | sed -n 1p; }

# ip_is_local reports whether an IPv4 address is assigned to a local interface.
ip_is_local(){ ip -4 -o addr show 2>/dev/null | awk '{print $4}' | sed 's#/.*##' | grep -Fx "$1" >/dev/null; }

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
# Default: the public IP chosen for this server (so a multi-IP box does not open
# the user ports on EVERY address — all IPv4 AND IPv6 — by accident, which was
# the old Enter behaviour). Typing 'all' (or 0.0.0.0) is the deliberate way to
# bind every address. When there is no local public IP to default to (a NAT'd
# server whose PUBIP is not on an interface), Enter still means all, since
# binding a non-local IP would just fail. A typed IP is validated like ask_bind_ip.
ask_user_ip(){
  local n def=""; n=$(local_ips | wc -l)
  # Default to the chosen public IP ONLY on a multi-IP server — that is where
  # Enter=ALL was an accident waiting to happen. On a single-IP box, keep the
  # old Enter=ALL so IPv6 inbound (which binding one IPv4 would drop) still works.
  [ "$n" -gt 1 ] && [ -n "${PUBIP:-}" ] && ip_is_local "$PUBIP" && def="$PUBIP"
  if [ "$n" -gt 1 ]; then echo >&2; info "IPs on this server (the user ports can open on one of them, or 'all'):"; show_ips; fi
  while :; do
    if [ -n "$def" ]; then
      read -rp "IP that USERS connect to on this server [$def; type 'all' for every IP incl. IPv6]: " USERIP </dev/tty
      USERIP=${USERIP:-$def}
    else
      read -rp "IP that USERS connect to on this server (Enter = ALL IPs, IPv4 and IPv6): " USERIP </dev/tty
    fi
    case "$USERIP" in
      ''|all|ALL|0.0.0.0|'*') USERIP=""
        [ "$n" -gt 1 ] && info "User ports open on all $n IPv4 addresses (and IPv6)."
        return 0 ;;
    esac
    if ip_is_local "$USERIP"; then ok "User ports open on $USERIP only."; return 0; fi
    warn "$USERIP is not on this server. Pick one from the list, type 'all', or Enter for ${def:-all}."
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
# matters.
# verify_download FILE [url-suffix] — check a download against its published
# sha256. Exit status:
#   0  the hash was fetched and the file matches it;
#   1  the hash was fetched but the file does NOT match (tampered/corrupt);
#   2  no usable hash could be fetched (the .sha256 is missing — an older repo —
#      OR the request was blocked/dropped on the path).
# install_binary treats BOTH 1 and 2 as a failure and retries once past the CDN
# cache; if it still does not verify, the binary is NOT installed. A middlebox
# that alters the binary and then drops the .sha256 to pass it off as "no hash"
# is therefore caught, not waved through. The only way to install on a 2 is to
# set HS2_ALLOW_UNVERIFIED=1 (an older repo that genuinely ships no hash).
# hs2_is_v3 FILE — true if FILE is an hs2 v3 binary. Anchored so a future
# "hs2 v30" can never be accepted as "hs2 v3".
hs2_is_v3(){ "$1" version 2>/dev/null | grep -qE 'hs2 v3([^0-9]|$)'; }

# bin_build FILE — print the compact build stamp ("build <rev>[+] <date>") that a
# stamped hs2 binary appends to its version line in [square brackets], or nothing
# for an older/unstamped binary or a missing file. Metadata only: it lets an
# operator eyeball that both ends of a tunnel run the same build. Must ALWAYS
# return 0 (empty output on any failure): it is used in a `bld=$(bin_build …)`
# assignment, and under `set -euo pipefail` a non-executable path or a failing
# `version` would otherwise propagate through the pipe and abort the caller. The
# `[ -x ]` guard and the trailing `|| true` keep it safe in every case.
bin_build(){
  [ -x "$1" ] || return 0
  # head -n1: even if a binary ever emitted the stamp on more than one line, the
  # banner stays a single line (the documented format is already one line).
  "$1" version 2>/dev/null | sed -n 's/.*\[\(build [^]]*\)\].*/\1/p' | head -n1 || true
}

verify_download(){ # file [url-suffix]
  local raw want got
  raw=$(curl -fsSL --connect-timeout 10 --retry 2 "$REPO_RAW/hs2-linux-amd64.sha256${2:-}" 2>/dev/null || true)
  # Accept coreutils ("<hash>  file") or BSD ("SHA256 (file) = <hash>"), any
  # case, CRLF or LF: pull the 64-hex token and lower-case it to match sha256sum.
  want=$(printf '%s' "$raw" | grep -oE '[0-9a-fA-F]{64}' | head -1 | tr 'A-F' 'a-f' || true)
  [ -n "$want" ] || return 2
  got=$(sha256sum "$1" | cut -d' ' -f1)
  if [ "$got" = "$want" ]; then ok "sha256 matches the published hash."; return 0; fi
  err "sha256 mismatch: published $want, downloaded $got"
  return 1
}

install_binary(){ # [force]
  # A working v3 binary on this server is SHARED by every tunnel's service. A
  # new tunnel's setup must NOT silently swap it for a freshly downloaded build
  # the running tunnels were never tested against — the next restart or crash of
  # a busy production tunnel would then bring it up on an untested binary. So
  # when setup finds a good binary already installed, default to KEEPING it;
  # only 'upgrade' (which passes "force" and then restarts every tunnel, each
  # verified) replaces it deliberately.
  local force="${1:-}"
  if [ "$force" != force ] && [ -x "$BIN" ] && hs2_is_v3 "$BIN"; then
    local cur ans=""
    cur=$("$BIN" version 2>/dev/null)
    hr
    info "hs2 is already installed on this server (one binary, shared by every tunnel):"
    say "    $cur"
    say "    Keep it, and this new tunnel runs the SAME binary the others already"
    say "    run — recommended. Updating replaces it for ALL tunnels at once; the"
    say "    Upgrade menu is the safe way to do that (it restarts and checks each)."
    # 2>/dev/null before </dev/tty so a host with no controlling terminal skips
    # the prompt silently; the empty default then KEEPS the binary (the safe
    # choice). HS2_YES=1 answers "update" for unattended runs.
    if [ "${HS2_YES:-}" = 1 ]; then ans=y
    else read -rp "Update the shared binary from GitHub now? [y/N]: " ans 2>/dev/null </dev/tty || true; fi
    case "$ans" in
      [yY]|[yY][eE][sS]) info "Updating the shared binary from GitHub…" ;;
      *) ok "Keeping the installed hs2."; install_self; return 0 ;;
    esac
  fi
  local d tmp; tmp=$(mktemp)
  d=$(cd "$(dirname "$0")" 2>/dev/null && pwd || pwd)
  # Download the latest from GitHub first, so a stale binary lying around can
  # never be reinstalled by mistake. Only if GitHub is unreachable (Iran
  # server) fall back to a hs2-linux-amd64 next to install.sh or in the
  # current directory. ALWAYS replace the old binary.
  info "Downloading hs2 binary from GitHub…"
  if curl -fL --connect-timeout 10 --retry 2 -o "$tmp" "$REPO_RAW/hs2-linux-amd64" 2>/dev/null; then
    ok "Downloaded."
    local rc=0
    verify_download "$tmp" || rc=$?
    if [ "$rc" != 0 ]; then
      # rc=1 (mismatch) or rc=2 (no hash fetched). Right after a release the CDN
      # can briefly serve the new binary with the old hash; and a middlebox can
      # alter the binary and then DROP the .sha256 request to pass it off as
      # "no hash". So fetch both once more past the CDN cache and fail CLOSED:
      # if it still does not verify — a mismatch OR the hash cannot be fetched —
      # the binary is NOT installed.
      local q; q="?v=$(date +%s)"
      warn "Verifying once more past the CDN cache…"
      rc=1
      if curl -fL --connect-timeout 10 --retry 2 -o "$tmp" "$REPO_RAW/hs2-linux-amd64$q" 2>/dev/null; then
        rc=0; verify_download "$tmp" "$q" || rc=$?
      fi
      if [ "$rc" = 2 ] && [ "${HS2_ALLOW_UNVERIFIED:-}" = 1 ]; then
        warn "No published sha256 to verify against; installing UNVERIFIED (HS2_ALLOW_UNVERIFIED=1)."
      elif [ "$rc" != 0 ]; then
        rm -f "$tmp"
        die "the downloaded binary could not be verified against its published sha256 (a mismatch, or the hash could not be fetched) — NOT installed. Try again in a few minutes; if it persists, something on the path is altering the download. An older repo that genuinely ships no hash: re-run with HS2_ALLOW_UNVERIFIED=1."
      fi
    fi
  elif [ -f "$d/hs2-linux-amd64" ] || [ -f "./hs2-linux-amd64" ]; then
    local f="$d/hs2-linux-amd64"; [ -f "$f" ] || f="./hs2-linux-amd64"
    warn "GitHub unreachable — using local $f (make sure it is the NEW one)."
    cp "$f" "$tmp"
  elif [ -x "$BIN" ] && hs2_is_v3 "$BIN"; then
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
  hs2_is_v3 "$tmp" \
    || { rm -f "$tmp"; die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."; }
  # No tunnel is stopped here: the rename below swaps the file (the old inode
  # lives on), so running tunnels keep the old binary until their own restart.
  # The tunnel being set up restarts at the end; the others move with Upgrade.
  # Stage beside $BIN then rename, so a failed or partial copy never leaves a
  # half-written $BIN that the exit trap would then try to start.
  local new="$BIN.new.$$"
  if ! install -m755 "$tmp" "$new"; then rm -f "$tmp" "$new"; die "could not stage the new binary (disk full, or $BIN not writable)."; fi
  rm -f "$tmp"
  if ! mv -f "$new" "$BIN"; then rm -f "$new"; die "could not install the new binary to $BIN."; fi
  # The full hash: compare it between the two servers (the Iran side may have
  # been given a copy by hand).
  info "sha256: $(sha256sum "$BIN" | cut -d' ' -f1)"
  hs2_is_v3 "$BIN" || die "binary is outdated or corrupt (need hs2 v3). Re-download hs2-linux-amd64."
  ok "Installed $("$BIN" version 2>/dev/null)"
  install_self
}

# ---------- tunnels as services ------------------------------------------------
# unit_cfg UNIT -> its config path by convention (hs2 keeps the historic name).
unit_cfg(){ if [ "$1" = hs2 ]; then echo "$CFG_DIR/config.json"; else echo "$CFG_DIR/$1.json"; fi; }

# use_unit UNIT points UNIT/CFG/SVC at one tunnel. An existing tunnel keeps the
# config its unit file actually runs (ExecStart -c ...), whatever its name.
use_unit(){ # unit
  UNIT="$1"; SVC="$UNIT_DIR/$1.service"
  CFG=$(tm_cfg "$1"); [ -n "$CFG" ] || CFG=$(unit_cfg "$1")
}

# norm_unit NAME -> the tunnel's service name, or failure for a bad name.
# Enter/"hs2" is the default tunnel; anything else becomes hs2-<name>, so a
# tunnel can never overwrite an unrelated system service (a name like "ssh").
norm_unit(){ # name
  local n; n=$(printf '%s' "$1" | tr 'A-Z' 'a-z' | tr -d '[:space:]')
  n=${n%.service}
  case "$n" in ''|hs2) echo hs2; return 0 ;; esac
  n=${n#hs2-}
  case "$n" in ''|*[!a-z0-9-]*|-*|*-|menu) return 1 ;; esac
  [ ${#n} -le 20 ] || return 1
  echo "hs2-$n"
}

unit_exists(){ [ -f "$UNIT_DIR/$1.service" ]; }

# unit_desc UNIT -> one line saying what that tunnel is.
unit_desc(){ # unit
  local c; c=$(tm_cfg "$1")
  if [ -f "$c" ]; then echo "$(tm_role "$c") · $(tm_dir "$c") · $(tm_transport "$c") · $(tm_endpoint "$c")"
  else echo "config missing"; fi
}

# free_unit_name BASE -> BASE, or BASE-2, BASE-3 … whichever is not taken.
free_unit_name(){ # base
  local b="$1" i=2
  unit_exists "$b" || { echo "$b"; return 0; }
  while unit_exists "$b-$i"; do i=$((i + 1)); done
  echo "$b-$i"
}

# list_tunnels prints the tunnels already on this server (for the name prompt).
list_tunnels(){
  local u n=0
  for u in $(tm_units); do
    [ "$n" = 0 ] && info "Tunnels already on this server:"
    n=$((n + 1)); say "     $u — $(unit_desc "$u")"
  done
  return 0
}

# ask_service_name: the side that MAKES the link names the tunnel. Enter keeps
# the default "hs2"; a name like "de1" runs as service hs2-de1 next to the
# others. The name travels in the link, so the other server uses the same one.
# Re-using a name replaces that tunnel (asked first; its tun subnet and
# interface are kept so routes that point at them keep working).
ask_service_name(){
  local n u a
  echo >&2
  info "Service name: every tunnel runs as its own service, so several can run side by side."
  list_tunnels
  while :; do
    read -rp "Service name for this tunnel [hs2] (e.g. de1 → hs2-de1): " n </dev/tty
    if ! u=$(norm_unit "$n"); then
      warn "Use letters, digits and '-' only (up to 20, not starting or ending with '-')."
      continue
    fi
    if unit_exists "$u"; then
      warn "A tunnel named $u already exists here: $(unit_desc "$u")"
      read -rp "Replace it with this new tunnel? [y/N]: " a </dev/tty
      case "$a" in y|Y|yes) REPLACING=1 ;; *) info "Pick another name then."; continue ;; esac
    else
      REPLACING=0
    fi
    break
  done
  use_unit "$u"
  ok "Service: $UNIT (config $CFG)"
  stop_for_replace
}

# adopt_service_name: the side that PASTES the link takes the name from it. If
# a tunnel of that name already exists here, it is either the same tunnel being
# set up again (it talks to the same server: replace it — the default then) or
# another one (keep it and give this one a free name — the default then).
adopt_service_name(){
  local u a same=0 old oh lh n
  u=$(norm_unit "${LNAME:-hs2}") || u=hs2
  REPLACING=0
  if ! unit_exists "$u"; then
    use_unit "$u"; ok "Service name from the link: $UNIT"; return 0
  fi
  old=$(jget "$(tm_cfg "$u")" addr); oh=${old%:*}; lh=${ENDPOINT%:*}
  [ -n "$oh" ] && [ "$oh" = "$lh" ] && same=1
  echo >&2
  warn "The link names this tunnel $u, and a tunnel with that name already exists here:"
  say "     $u — $(unit_desc "$u")"
  if [ "$same" = 1 ]; then
    info "It talks to the same server ($lh), so this is most likely the same tunnel set up again."
  fi
  say "    1) Replace it with this one"
  say "    2) Keep it, and run this one under another name"
  read -rp "Choose [$([ "$same" = 1 ] && echo 1 || echo 2)]: " a </dev/tty
  a=${a:-$([ "$same" = 1 ] && echo 1 || echo 2)}
  case "$a" in
    1) REPLACING=1 ;;
    2) while :; do
         read -rp "Service name for this tunnel here [$(free_unit_name "$u")]: " n </dev/tty
         n=${n:-$(free_unit_name "$u")}
         if ! u=$(norm_unit "$n"); then warn "Use letters, digits and '-' only (up to 20)."; continue; fi
         if unit_exists "$u"; then warn "$u exists too — pick another name."; continue; fi
         break
       done ;;
    *) die "invalid choice" ;;
  esac
  use_unit "$u"
  ok "Service: $UNIT (config $CFG)"
  stop_for_replace
}

# stop_for_replace: a tunnel that is being set up again still holds its ports
# (tunnel port, user ports), so it is stopped now — only that one. If the setup
# ends early (an error, Ctrl+C) the safety net starts it again on its old
# config; a finished setup restarts it on the new one.
stop_for_replace(){
  [ "${REPLACING:-0}" = 1 ] || return 0
  [ "$(systemctl is-active "$UNIT" 2>/dev/null || true)" = active ] || return 0
  HS2_STOPPED_UNITS="$HS2_STOPPED_UNITS $UNIT"
  # Stash the CURRENT (old) config: the setup overwrites $CFG in place, so if it
  # aborts after that, the safety net must restart the tunnel on the OLD config,
  # not the half-written new one. start_service removes the stash once the new
  # config is live; on_exit restores it if the setup never got that far — but
  # only because we record the unit here, so on_exit knows THIS run owns it.
  if [ -f "$CFG" ] && cp -p "$CFG" "$CFG.pre-replace" 2>/dev/null; then
    HS2_STASHED_UNITS="$HS2_STASHED_UNITS $UNIT"
  fi
  info "Stopping $UNIT for the new setup (it is started again if the setup is cancelled)…"
  systemctl stop "$UNIT" 2>/dev/null || true
}

# ---------- per-tunnel tun subnet and interface -------------------------------
# set_tun_subnet BASE: the tunnel's /30 — iran = BASE+1, kharej = BASE+2. The
# last octet is forced base-10 (10#) so a non-canonical octet never makes the
# arithmetic read it as octal and abort under set -e.
set_tun_subnet(){ # base (a.b.c.d, d a multiple of 4)
  local p=${1%.*} l=${1##*.}
  TUN_BASE="$1"
  TUN_IP_IRAN="$p.$((10#$l + 1))"; TUN_IP_KHAREJ="$p.$((10#$l + 2))"
  TUN_SUBNET_IRAN="$TUN_IP_IRAN/30"; TUN_SUBNET_KHAREJ="$TUN_IP_KHAREJ/30"
  TUN_PEER_IRAN="$TUN_IP_KHAREJ"; TUN_PEER_KHAREJ="$TUN_IP_IRAN"
}

# block_of IP -> the /30 it belongs to. 10# forces base-10 so a leading-zero
# octet can't abort the math; a non-numeric/empty last octet (a hand-edited
# config reaching this via used_blocks) yields a harmless .0 rather than aborting
# the whole installer under set -e with a raw arithmetic error.
block_of(){
  local l=${1##*.}
  case "$l" in ''|*[!0-9]*) echo "${1%.*}.0"; return 0 ;; esac
  echo "${1%.*}.$(( 10#$l / 4 * 4 ))"
}

# valid_block BASE: a /30 base inside 10.77.0.0/16.
valid_block(){ # base
  local a b c d o
  IFS=. read -r a b c d <<<"$1"
  [ "$a" = 10 ] && [ "$b" = 77 ] || return 1
  # Each of the last two octets must be all-digit, non-empty and carry NO
  # superfluous leading zero: a value like "08" is read as octal by the $(())
  # below and aborts the installer under set -e (and "10.77.0.08" is not a
  # canonical base anyway). "0" alone is allowed.
  for o in "$c" "$d"; do
    case "$o" in ''|*[!0-9]*) return 1 ;; 0) ;; 0*) return 1 ;; esac
  done
  [ "$c" -le 255 ] && [ "$d" -le 252 ] && [ $((d % 4)) = 0 ]
}

# used_blocks: the /30s taken on this server — by the other tunnels' configs
# (running or not) and by any interface address — except the tunnel being
# replaced ($UNIT when REPLACING=1), whose subnet is free to reuse.
used_blocks(){
  local u c lc own="" ifc a
  for u in $(tm_units); do
    [ "$u" = "$UNIT" ] && [ "${REPLACING:-0}" = 1 ] && continue
    c=$(tm_cfg "$u"); lc=$(jget "$c" local_cidr)
    [ -n "$lc" ] && block_of "${lc%/*}"
  done
  [ "${REPLACING:-0}" = 1 ] && own=$(cfg_tun_iface "$CFG")
  ip -4 -o addr show 2>/dev/null | awk '{print $2, $4}' | while read -r ifc a; do
    [ -n "$own" ] && [ "$ifc" = "$own" ] && continue
    case "$a" in 10.77.*) block_of "${a%/*}" ;; esac
  done
  return 0
}

# pick_subnet (the side that makes the link): the tunnel being replaced keeps
# its subnet; a new one gets a random free /30 in 10.77.0.0/16 — random, so two
# servers that each make a link for the same third server almost never pick the
# same one (the side that pastes checks anyway). 10.77.0.0/30 stays for the
# default tunnel of older installs.
pick_subnet(){
  local used b i=0 lc
  if [ "${REPLACING:-0}" = 1 ]; then
    lc=$(jget "$CFG" local_cidr)
    if [ -n "$lc" ] && valid_block "$(block_of "${lc%/*}")"; then set_tun_subnet "$(block_of "${lc%/*}")"; return 0; fi
  fi
  used=$(used_blocks)
  while [ $i -lt 500 ]; do
    i=$((i + 1))
    b="10.77.$(( RANDOM % 256 )).$(( (RANDOM % 64) * 4 ))"
    [ "$b" = 10.77.0.0 ] && continue
    printf '%s
' "$used" | grep -Fqx "$b" && continue
    set_tun_subnet "$b"; return 0
  done
  die "could not find a free tunnel subnet in 10.77.0.0/16"
}

# ask_subnet (link-making side): establish the default subnet with pick_subnet
# (a fresh random /30, or the reused one when REPLACING), then let the operator
# override it with a specific /30 base. Random stays the DEFAULT — an empty
# answer keeps pick_subnet's choice. A chosen base is useful when routes or
# monitoring on the other server are pinned to a known tunnel IP (the U5 case:
# a silently-changed subnet breaks them). The base travels in the link, so the
# pasting side re-validates it independently via check_link_subnet.
ask_subnet(){
  local ans b used
  pick_subnet                       # sets TUN_BASE to the default (random / reused)
  while :; do
    read -rp "Tunnel subnet /30 base in 10.77.0.0/16 [$TUN_BASE; Enter = keep]: " ans </dev/tty || return 0
    [ -n "$ans" ] || return 0       # empty keeps the default -> random stays default
    b=${ans%/*}                     # accept "10.77.42.0" or "10.77.42.0/30"
    if ! valid_block "$b"; then warn "Enter a /30 base inside 10.77.0.0/16 (last octet a multiple of 4), e.g. 10.77.42.0."; continue; fi
    used=$(used_blocks)             # capture first (pipefail/SIGPIPE), same idiom as check_link_subnet
    if printf '%s\n' "$used" | grep -Fqx "$b"; then warn "$b/30 is already in use on this server — pick another, or press Enter for the default."; continue; fi
    set_tun_subnet "$b"; ok "Tunnel subnet set to $b/30."; return 0
  done
}

# check_link_subnet (the side that pastes): the subnet from the link must be
# free here too — two tunnels on one subnet would break each other.
check_link_subnet(){
  local b="${LSUBNET:-10.77.0.0}" u c lc by="" used
  valid_block "$b" || die "the link carries an invalid tunnel subnet ($b) — make a new link on the other server."
  # Capture used_blocks before matching: piping it straight into `grep -q` lets
  # grep exit on the first match and SIGPIPE the (multi-fork) producer, which
  # under `set -o pipefail` makes the pipeline report failure — i.e. "not used" —
  # so a real collision is missed about half the time. Same idiom as pick_subnet.
  used=$(used_blocks)
  if printf '%s\n' "$used" | grep -Fqx "$b"; then
    for u in $(tm_units); do
      [ "$u" = "$UNIT" ] && [ "${REPLACING:-0}" = 1 ] && continue
      c=$(tm_cfg "$u"); lc=$(jget "$c" local_cidr)
      if [ -n "$lc" ] && [ "$(block_of "${lc%/*}")" = "$b" ]; then by=$u; break; fi
    done
    if [ -n "$by" ]; then err "The link's tunnel subnet $b/30 is already used here by tunnel $by ($(unit_desc "$by"))."
    else err "The link's tunnel subnet $b/30 is already used by an interface on this server."; fi
    die "Run the setup again on the OTHER server (it picks a new random subnet) and paste the new link. Nothing was changed here."
  fi
  set_tun_subnet "$b"
}

# cfg_tun_iface CFG -> the tun interface that tunnel really creates. Empty for
# carrier mtcp: it runs without one, although its config names one (older
# installs wrote "hs0" there) — so it never reserves, lists or deletes a name
# another tunnel's real interface may carry.
cfg_tun_iface(){ # cfg
  [ "$(jget "$1" carrier)" = mtcp ] && return 0
  jget "$1" iface
}

# used_ifaces: tun interface names the other tunnels' configs use.
used_ifaces(){
  local u c
  for u in $(tm_units); do
    [ "$u" = "$UNIT" ] && [ "${REPLACING:-0}" = 1 ] && continue
    c=$(tm_cfg "$u"); cfg_tun_iface "$c"
  done
  return 0
}

# iface_taken NAME: another tunnel uses it, or it is an interface that is not
# the replaced tunnel's own.
iface_taken(){ # name
  local own="" used
  [ "${REPLACING:-0}" = 1 ] && own=$(cfg_tun_iface "$CFG")
  used=$(used_ifaces) # capture first: `used_ifaces | grep -q` can SIGPIPE the producer under pipefail and miss a match
  printf '%s\n' "$used" | grep -Fqx "$1" && return 0
  [ "$1" != "$own" ] && ip link show "$1" >/dev/null 2>&1
}

# free_iface -> the interface this tunnel should use: the replaced tunnel's
# own, else the first free of hs0, hs1, …
free_iface(){
  local i=0 own="" used
  [ "${REPLACING:-0}" = 1 ] && own=$(cfg_tun_iface "$CFG")
  if [ -n "$own" ]; then
    used=$(used_ifaces) # capture first (see iface_taken): avoids the pipefail/SIGPIPE false negative
    if ! printf '%s\n' "$used" | grep -Fqx "$own"; then echo "$own"; return 0; fi
  fi
  while iface_taken "hs$i"; do i=$((i + 1)); done
  echo "hs$i"
}

# unit_unmask UNIT: a MASKED unit is a symlink to /dev/null (in /etc, or in /run
# for a runtime mask). Writing the unit file then goes nowhere or is overridden,
# `enable`/`restart` fail, and an instance started BEFORE the mask keeps running
# the OLD config while still reporting "active" — so a new config would silently
# never load. This installer never masks hs2; if something else did, undo it
# loudly so the config we write is the config that runs.
unit_unmask(){ # unit
  local u="$1" f="$UNIT_DIR/$1.service"
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
  unit_unmask "$UNIT"   # before writing: a masked unit file is a /dev/null symlink
  # StartLimit* moved from [Service] to [Unit] in systemd 230. On older systemd
  # the [Unit] form is silently ignored, so the tunnel would stop being restarted
  # after the default burst (5 in 10 s) — breaking "never give up". Emit the form
  # that matches THIS systemd (default to the modern [Unit] form when unknown).
  local sdver sd_unit="StartLimitIntervalSec=0" sd_service=""
  sdver=$(systemctl --version 2>/dev/null | awk 'NR==1{print $2}')
  case "$sdver" in
    ''|*[!0-9]*) ;;                                              # unknown → modern form
    *) [ "$sdver" -lt 230 ] && { sd_unit=""; sd_service="StartLimitInterval=0"; } ;;
  esac
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
Description=hs2 tunnel $UNIT ($role)
Documentation=https://github.com/aliyaghoobi2323-cloud/hs2-
After=network-online.target
Wants=network-online.target
$sd_unit

[Service]
Type=simple
$sd_service
ExecStartPre=-/sbin/modprobe tun
ExecStart=$BIN run -c $CFG
# reload = hot-swap the TLS certificate (SIGHUP) without dropping the tunnel;
# the certbot renewal hook sends SIGHUP to every hs2 tunnel.
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

# write_cfg_checked: read a tunnel config JSON on stdin, VALIDATE it with
# `hs2 check`, then install it to $CFG atomically. The setup-time counterpart of
# the atomic binary install (phase A1): a malformed interpolation — an unescaped
# character, a bad number — can no longer produce a crash-looping tunnel started
# from a half-written, invalid config, because the unit is never enabled on a
# config that does not parse. On an OLD binary without `check` it falls back to a
# python3 JSON-syntax check, and if that is unavailable too, to a plain write
# (exactly today's behavior). The file is created mode 600 (umask 177) so the
# tunnel key is never briefly world-readable.
write_cfg_checked(){
  local new="$CFG.new.$$" out rc=0
  mkdir -p "$(dirname "$CFG")"
  ( umask 177; cat > "$new" ) || { rm -f "$new"; die "could not write $new (disk full, or $(dirname "$CFG") not writable)."; }
  out=$("$BIN" check -c "$new" 2>&1) || rc=$?
  if printf '%s' "$out" | grep -q "unknown command"; then
    if command -v python3 >/dev/null 2>&1; then
      python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$new" >/dev/null 2>&1 \
        || { rm -f "$new"; die "the config did not come out as valid JSON — not installed. Re-run setup and check the values you entered (a domain/panel/port)."; }
    fi
  elif [ "$rc" != 0 ]; then
    # The engine (hs2 check) rejected it. The reason is already printed via err;
    # it is almost always an entered value it does not accept (e.g. an ipx proto
    # number reserved for a real protocol, or a panel/port it cannot parse), not
    # an installer fault — so tell the operator to re-run and pick another value.
    err "$out"; rm -f "$new"
    die "the config was rejected by 'hs2 check' (see the error above) — not installed. Re-run setup and enter a different value."
  fi
  mv -f "$new" "$CFG" || { rm -f "$new"; die "could not install the config to $CFG."; }
}

start_service(){
  local role="$1"
  PEER_UNVERIFIED=0   # set to 1 below if the post-setup peer check fails
  mkdir -p "$(dirname "$CFG")"; chmod 700 "$(dirname "$CFG")" 2>/dev/null || true
  unit_unmask "$UNIT"
  systemctl enable "$UNIT" >/dev/null 2>&1 || true
  if restart_unit "$UNIT"; then
    ok "$UNIT ($role) is running with the new config."
    ok "Autostart on boot: ON — it comes back by itself after a reboot or crash."
    info "Manage it any time with:  hs2-menu   → 3) Tunnel manager → $UNIT"
  else
    err "$UNIT did not start with the new config. Last log:"
    journalctl -u "$UNIT" -n 20 --no-pager >&2 || true
    bail
  fi
  # The side that pasted the link starts second: the other server is already
  # waiting, so the tunnel must connect NOW. A running service is not proof —
  # a raw encapsulation the path filters (gre/ipip often are) runs happily and
  # carries nothing — so nothing says "ready" until packets really cross.
  if [ "${VERIFY_PEER:-0}" = 1 ] && ! verify_tunnel "$CFG" "${HS2_VERIFY_SECS:-60}"; then
    PEER_UNVERIFIED=1
    tunnel_down_help "$CFG"
    # The service is installed and RUNNING (it was health-checked just above) and
    # retries on its own, so this is NOT a failed install — do not bail. On a
    # high-latency Iran path the peer can still be coming up; if it connects later
    # (the other side finishes, or the path opens) the tunnel comes up with no
    # re-install. Report it and point at the live status instead of exiting hard.
    warn "$UNIT is installed and running, and keeps trying on its own. Re-check once the OTHER server is up:"
    warn "   hs2-menu → 3) Tunnel manager → $UNIT   (it shows ✓ connected when the peer answers)"
  fi
  # The new config is live now, so a REPLACING setup's stash of the OLD config is
  # no longer a fallback — drop it (the exit trap must not roll back on success).
  rm -f "$CFG.pre-replace"
}

# dialer_done MSG: the dialer's final line, right after start_service. The dialer
# verifies the peer (VERIFY_PEER=1); if that FAILED, start_service already printed
# the red "did NOT connect" diagnosis, so a green "… ready" here would contradict
# it — say it is installed-but-not-yet-connected instead, matching what was shown.
dialer_done(){ # message
  if [ "${PEER_UNVERIFIED:-0}" = 1 ]; then
    info "$UNIT is installed and running but is NOT connected yet (see above) — it keeps trying on its own."
  else
    ok "$1"
  fi
}

# tunnel_up CFG: 0 when the tunnel really carries packets to the other server.
# Every carrier counts a link only after the peer answered its authenticated
# handshake, so a live link in the daemon's status file is proof; the carriers
# without one (udp/auto) are proven by the peer's tun IP answering ping.
tunnel_up(){ # cfg [list]
  local cfg="$1" mode="${2:-}" sf links ifc peer car c=1
  car=$(jget "$cfg" carrier); ifc=$(jget "$cfg" iface); peer=$(jget "$cfg" peer_ip)
  # A tunnel with a tun interface is proven by a packet crossing it and coming
  # back: ping the peer's tun IP. A link count is NOT proof for it — a path
  # that lets a handshake through and then kills the flow (the Iran border
  # does this to UDP) leaves carriers counted "up" with nothing crossing.
  if [ "$car" != mtcp ] && [ -n "$ifc" ] && [ -n "$peer" ]; then
    # -W2 (not 1): tolerate a high-latency Iran↔foreign path so a working tunnel
    # that answers a little slowly is not read as down. ping returns 0 on the
    # first reply, so an UP tunnel stays fast; only a down one waits.
    # In "list" mode send TWO probes so a single dropped echo on a lossy path
    # (exactly the environment the datagram carriers target) does not paint a
    # healthy tunnel red; verify_tunnel's own loop already provides retries.
    [ "$mode" = list ] && c=2
    ip link show "$ifc" >/dev/null 2>&1 && ping -c"$c" -W2 -I "$ifc" "$peer" >/dev/null 2>&1
    return $?
  fi
  # mtcp has no tun: a link counts only after the peer proved the key over it.
  sf=$(status_path "$cfg")
  status_fresh "$sf" || return 1
  links=$(jraw "$sf" links)
  [ "${links:-0}" -gt 0 ] 2>/dev/null
}

# tunnel_conn_label CFG: whether the tunnel really reaches the other server
# right now (tunnel_up: live authenticated links, or the peer's tun IP answers)
# — "the service runs" and "its interface is up" say nothing about that.
tunnel_conn_label(){ # cfg
  local sf links
  if tunnel_up "$1"; then
    sf=$(status_path "$1"); links=""
    status_fresh "$sf" && links=$(jraw "$sf" links)
    printf '%s✓ connected%s%s' "$C_G" "$C_0" "$([ -n "$links" ] && [ "$links" != 0 ] && echo " ($links links)")"
  else
    printf '%s✗ NOT connected%s — nothing comes back from the other server' "$C_R" "$C_0"
  fi
}

# verify_tunnel CFG SECS: wait up to SECS for tunnel_up, probing every 2 s (so a
# default 60 s window is ~30 probes — robust to a lossy path, since one reply is
# enough). The default is generous because the peer may still be coming up,
# especially on a slow Iran↔foreign path or before the other side is upgraded.
verify_tunnel(){ # cfg secs
  local secs=${2:-60} end
  end=$(( $(date +%s) + secs ))
  info "Checking that the tunnel really reaches the other server (up to ${secs} s)…"
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
  warn "Also check:"
  warn " · the shared key: the link you pasted must be the other server's CURRENT one — running its setup"
  warn "   again makes a NEW key, and with a different key the other side stays silent (or shows a website);"
  warn " · the other server finished its setup and its tunnel runs (hs2-menu → Tunnel manager there);"
  warn " · its tunnel port is open in its firewall / provider panel, and the dial-out IP here is not filtered."
  info "hs2 stays installed and keeps retrying — if the path opens later it connects by itself."
  info "Last log:"
  journalctl -u "$UNIT" -n 15 --no-pager -o cat 2>/dev/null | sed 's/^/     /' >&2 || true
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
    # Only the Address lines in the ANSWER section (after "Name:"); the earlier
    # Address line is the RESOLVER's own IP (often with a #53 suffix), not an A
    # record. Strip any #port.
    nslookup -type=A "$d" 2>/dev/null | awk 'tolower($1)=="name:"{a=1;next} a&&tolower($1)=="address:"{ip=$2;sub(/#.*/,"",ip);print ip}'
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
  if printf '%s\n' "$got" | grep -Fqx "$ip"; then
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
       --register-unsafely-without-email --deploy-hook "$CERT_HOOK" >/dev/null 2>&1; then
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
       --register-unsafely-without-email --deploy-hook "$CERT_HOOK" </dev/tty >&2; then
    ok "Certificate obtained for $domain via DNS-01."
  else
    err "certbot DNS-01 did not complete for $domain."
    warn "Obtain a certificate another way and re-run choosing 'existing certificate'."
    die  "certificate not obtained."
  fi
  configure_renewal "$domain"
  printf '/etc/letsencrypt/live/%s/fullchain.pem|/etc/letsencrypt/live/%s/privkey.pem' "$domain" "$domain"
}

# CERT_HOOK runs after every renewal: SIGHUP to every running hs2 tunnel, which
# hot-swaps the certificate of those that have one and is ignored by the rest.
# One hook for all tunnels, so adding or deleting a tunnel never leaves a
# renewal pointing at a service that is gone (it used to be `systemctl reload
# hs2`, which only reached the default tunnel).
CERT_HOOK="pkill -HUP -x hs2"

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
    sed -i "s#^renew_hook.*#renew_hook = $CERT_HOOK#" "$conf"
  elif grep -q '^\[renewalparams\]' "$conf"; then
    sed -i "/^\[renewalparams\]/a renew_hook = $CERT_HOOK" "$conf"
  else
    printf '[renewalparams]\nrenew_hook = %s\n' "$CERT_HOOK" >> "$conf"
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
  echo "    1) mtcp + tun — the mtcp multi-link pool (2–32 TLS links) + a tun interface (recommended, fastest)" >&2
  echo "    2) tls  + tun — one TLS link + a tun interface (fewer connections, but far slower where each connection is throttled)" >&2
  echo "    (Users' traffic rides the user ports. The tun here is a side channel for ping and light" >&2
  echo "     traffic, not for bulk — for bulk over a routed tun choose tun → udp or icmp.)" >&2
  read -rp "Choose [1]: " M </dev/tty
  case "${M:-1}" in 1) CARRIER=l3mtcp ;; 2) CARRIER=tls ;; *) die "invalid mode" ;; esac
}

# tun_tls_carrier normalizes the carrier a tun -> tcp link carries: l3mtcp or
# tls; anything else (an old link) is the multi-link pool, as it always was.
tun_tls_carrier(){ case "${CARRIER:-}" in tls) CARRIER=tls ;; *) CARRIER=l3mtcp ;; esac; }

# tun_tls_label names the carrier for messages.
tun_tls_label(){ [ "$CARRIER" = tls ] && echo "one TLS link" || echo "mtcp multi-link pool"; }

# ---------- input validators -------------------------------------------------
# These guard the values that land in the tunnel config JSON and in the hs2://
# link, so one bad entry can never write invalid JSON (which breaks the service)
# or — because many of these travel in the link — break BOTH servers.

# valid_uint V MIN MAX — true if V is a base-10 integer in [MIN,MAX] with NO
# superfluous leading zero. Leading zeros must be rejected BEFORE any arithmetic
# test: under set -euo pipefail, `[ 08 -le N ]` is fine but a value like "08"
# emitted as a BARE JSON number ("mtu": 08) is invalid JSON. "0" alone is legal.
valid_uint(){ # value min max
  case "${1:-}" in
    ''|*[!0-9]*) return 1 ;;   # empty or non-digit
    0) ;;                      # a single zero is fine
    0*) return 1 ;;            # any other leading zero -> invalid/octal JSON
  esac
  # 2>/dev/null: a value longer than bash's integer width would otherwise leak
  # a raw "[: integer expression expected" — it is simply out of range, reject it.
  [ "$1" -ge "$2" ] 2>/dev/null && [ "$1" -le "$3" ] 2>/dev/null
}

# valid_domain NAME — true for a plausible hostname or IPv4 literal carrying no
# character that would break a JSON string or the '|'-delimited link ('"', '\',
# '|', spaces are all outside the allowed set).
valid_domain(){ # name
  case "${1:-}" in
    ''|-) return 1 ;;
    *[!a-zA-Z0-9.-]*) return 1 ;;   # letters, digits, dot, hyphen only
    .*|-*|*.|*-) return 1 ;;        # no leading/trailing dot or hyphen
    *..*) return 1 ;;               # no empty label
  esac
  [ "${#1}" -le 253 ]
}

# valid_hostport HP — true for a panel address host:port that is safe to put in
# a JSON string and in the '|'-delimited link, and that ends in a real port. The
# host grammar is deliberately permissive (hostname, IPv4, bracketed IPv6,
# underscore names a panel may use): the engine's `hs2 check` validates the
# address semantics, so the installer only has to keep out the characters that
# would break the JSON or the link ('"', '\', '|', whitespace) and require a
# numeric port, rather than being stricter than the engine it configures.
valid_hostport(){ # host:port
  local hp="${1:-}" port
  case "$hp" in
    '') return 1 ;;
    *'"'*|*'\'*|*'|'*|*[[:space:]]*) return 1 ;;   # JSON/link-breaking chars
    *:*) ;; *) return 1 ;;                          # must carry a :port
  esac
  port=${hp##*:}
  valid_uint "$port" 1 65535
}

# ask_domain PROMPT — read DOMAIN from the tty, require a valid hostname, re-ask
# on a bad entry (it is TLS-critical and travels in the link).
ask_domain(){ # prompt
  while :; do
    read -rp "$1" DOMAIN </dev/tty || die "a domain is required"
    valid_domain "$DOMAIN" && return 0
    warn "Enter a domain name (letters, digits, '.', '-' only), e.g. tunnel.example.com."
  done
}

# ask_panel PROMPT — read PANEL from the tty (default 127.0.0.1:8443), require a
# valid host:port, re-ask on a bad entry.
ask_panel(){ # prompt
  while :; do
    read -rp "$1" PANEL </dev/tty || die "a panel address is required"
    PANEL=${PANEL:-127.0.0.1:8443}
    valid_hostport "$PANEL" && return 0
    warn "Enter the panel address as host:port, e.g. 127.0.0.1:8443."
  done
}

# ask_ipx_proto sets TUN_PROTO: the raw IP protocol number the ipx encapsulation
# rides on. It must be the SAME number on both servers. 253 is the default
# (experimental range); 1/4/6/17/47 are taken by ICMP/IPIP/TCP/UDP/GRE. The
# installer only enforces the structural rule (0-255, no leading zero); the
# engine's `hs2 check` rejects numbers already assigned to a real protocol.
ask_ipx_proto(){
  read -rp "IPX raw IP protocol number (same on both servers) [253]: " IPXP </dev/tty
  IPXP=${IPXP:-253}
  valid_uint "$IPXP" 0 255 || die "protocol number must be an integer 0-255 with no leading zero (253 is the default)"
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
  local def; def=$(free_iface)
  while :; do
    read -rp "TUN interface name on THIS server [$def]: " TUNIF </dev/tty; TUNIF=${TUNIF:-$def}
    case "$TUNIF" in ''|*[!a-zA-Z0-9_-]*) warn "Use letters, digits, '_' and '-' only."; continue ;; esac
    [ ${#TUNIF} -le 15 ] || { warn "An interface name is at most 15 characters."; continue; }
    if iface_taken "$TUNIF"; then warn "$TUNIF is already used by another tunnel or interface here — pick another (Enter = $def)."; continue; fi
    break
  done
}
ask_tun_mtu(){
  read -rp "TUN MTU (1320 matches Backhaul; kept in sync with the other side) [1320]: " TUNMTU </dev/tty
  TUNMTU=${TUNMTU:-1320}
  # Bare JSON number + travels in the link: a leading zero ("01320") is invalid
  # JSON on BOTH ends. The range only rules out values that are not an MTU at all
  # (the engine itself just WARNs outside 1200-1500 and runs, so the installer
  # must not be stricter than it): 68 is the IPv4 minimum, 65535 the IP maximum.
  valid_uint "$TUNMTU" 68 65535 || die "MTU must be an integer 68-65535 with no leading zero (1320 is the default)"
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
  # Fields 12 and 13 are this tunnel's service name and tun subnet, so the
  # other server runs it under the same name on the same addresses.
  local L; L=$(encode_link "$1|$2|$3|$4|$5|$6|$7|$8|${9:-}|${10:-}|${11:-}|$UNIT|$TUN_BASE")
  echo >&2; hr
  ok "SETUP LINK — copy it to the OTHER server:"
  _c '1;33' "hs2://$L"
  hr
}

# parse_link reads a pasted hs2:// link into ENDPOINT DOMAIN SHARED PANEL
# CARRIER UDP TRANSPORT DIRECTION MTU ENCAP PROTO LNAME LSUBNET (with sensible
# defaults for older links; MTU/ENCAP/PROTO are only used by tun mode — PROTO is
# the ipx protocol number, so the other side never asks it; LNAME/LSUBNET are
# the tunnel's service name and tun subnet — an older link means the default
# tunnel "hs2" on 10.77.0.0/30, exactly what it always was).
parse_link(){
  read -rp "Paste the hs2:// setup link from the OTHER server: " RAW </dev/tty
  RAW=${RAW#hs2://}
  local DEC; DEC=$(decode_link "$RAW") || die "invalid link"
  IFS='|' read -r ENDPOINT DOMAIN SHARED PANEL CARRIER UDP TRANSPORT DIRECTION MTU ENCAP PROTO LNAME LSUBNET <<< "$DEC"
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
  # Sanitize the two numbers the link carries, so a hand-edited or third-party
  # link can never write invalid JSON on THIS side. PROTO blanks on a bad value
  # (ipx_proto_from_link then re-asks it, as for an older link that omits it).
  # MTU cannot be re-asked on the pasting side, so we only reject a value that
  # would be invalid JSON (non-numeric, or a leading zero); any valid-JSON
  # integer is kept verbatim — the engine accepts any MTU and only WARNs on an
  # unusual one, so the installer must not reject a value an already-issued link
  # legitimately carries (and `hs2 check` at write time catches a truly bad one).
  valid_uint "${PROTO:-}" 0 255 || PROTO=""
  case "${MTU:-}" in
    ''|-) MTU="" ;;
    *[!0-9]*) die "the link carries a non-numeric MTU ($MTU) — regenerate it on the OTHER server." ;;
    0|[1-9]*) : ;;   # valid JSON integer (0, or no leading zero) — keep as the peer set it
    *) die "the link carries a malformed MTU ($MTU, leading zero) — regenerate it on the OTHER server." ;;
  esac
  LNAME=${LNAME:-hs2}; LSUBNET=${LSUBNET:-10.77.0.0}
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
    ask_service_name     # this side makes the link, so it names the tunnel
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
  ask_subnet; TUNIF=$(free_iface)
  ask_tunnel_port "Tunnel port (clients never see this)"
  ask_bind_ip
  local SHARED; SHARED=$(openssl rand -hex 32)
  local PANEL="-" LMTU=""
  mkdir -p "$(dirname "$CFG")"

  if [ "$TRANSPORT" = "tcp" ]; then
    ask_panel "Panel inbound address on this server [127.0.0.1:8443]: "
    port_free "$TPORT" || die "TCP port $TPORT is already in use — pick another."
    ask_domain "Domain (its A record must point to $PUBIP): "
    echo >&2
    echo "  TLS mode:  1) mtcp (recommended)  2) l3mtcp  3) tls" >&2
    read -rp "Choose [1]: " M </dev/tty
    case "${M:-1}" in 1) CARRIER=mtcp ;; 2) CARRIER=l3mtcp ;; 3) CARRIER=tls ;; *) die "invalid mode" ;; esac
    read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
    case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    write_cfg_checked <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
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
    ask_panel "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: "
    ask_domain "Domain (its A record must point to $PUBIP): "
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; tun_tls_carrier
    read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
    case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    write_cfg_checked <<EOF
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
    ask_panel "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: "
    LMTU=1280; CARRIER=dgtun; DOMAIN="-"; UDP=false
    info "Tunnel MTU is fixed at 1280 here — it leaves room for the datagram path's obfuscation and FEC overhead and is the same on both ends automatically (tun-over-TCP asks for an MTU; the datagram carriers do not)."
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$TUN_ENCAP")
    write_cfg_checked <<EOF
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
    write_cfg_checked <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": false,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
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
    info "L3 tunnel on $TUNIF once up: this kharej = $TUN_IP_KHAREJ, iran = $TUN_IP_IRAN (MTU $LMTU)."
    info "Panel $PANEL receives the user ports you open on the iran side (asked there)."
  elif [ "$TRANSPORT" = "tun" ]; then
    info "Datagram L3 tunnel on $TUNIF once up: this kharej = $TUN_IP_KHAREJ, iran = $TUN_IP_IRAN (encap $TUN_ENCAP)."
    info "Panel $PANEL receives the user ports you open on the iran side (asked there)."
  fi
  info "On the Iran server: bash install.sh → 2 (Iran) → direction 'direct' → paste the link."
  info "It runs there as service $UNIT too (the name is in the link)."
  info "Then check this side any time:  hs2-menu → 3) Tunnel manager → $UNIT  — it shows ✓ connected once the other server is up."
  tunnel_summary
}

# kharej_dialer: reverse exit. Kharej DIALS the iran edge; it pastes the link
# iran generated (which carries iran's endpoint) and forwards to the panel.
kharej_dialer(){
  VERIFY_PEER=1   # the iran edge is already waiting: start_service proves the tunnel
  parse_link
  [ "$DIRECTION" = "reverse" ] || die "this link is a DIRECT link; for reverse, generate the link on the IRAN side first."
  adopt_service_name; check_link_subnet; TUNIF=$(free_iface)
  ask_egress_ip
  ok "Link OK — will dial the iran edge at $ENDPOINT (transport $TRANSPORT)."
  mkdir -p "$(dirname "$CFG")"
  if [ "$TRANSPORT" = "tcp" ]; then
    ask_panel "Panel inbound address on this server [127.0.0.1:8443]: "
    write_cfg_checked <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": true,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1380,
  "shared_key": "$SHARED",
  "expose": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    dialer_done "KHAREJ ready (reverse, tcp). It dials in to the Iran edge and forwards to $PANEL."
  elif [ "$TRANSPORT" = "tun" ] && [ "$ENCAP" = "tcp" ]; then
    # Reverse tun: kharej DIALS the iran edge (TLS client) and runs the L3 pipe.
    # MTU comes from the link so both sides match. The user ports iran opens
    # ride streams to the panel set here (expose), exactly like reverse tcp.
    [ -n "$MTU" ] || die "this link has no MTU field — regenerate it on the iran edge with the new installer."
    tun_tls_carrier   # the link says mtcp pool (l3mtcp) or one TLS link (tls)
    ask_tun_params
    ask_panel "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: "
    write_cfg_checked <<EOF
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
    dialer_done "KHAREJ ready (reverse, tun over TLS: $(tun_tls_label)). It dials in to the Iran edge and forwards to $PANEL."
    info "L3 tunnel on $TUNIF once up: this kharej = $TUN_IP_KHAREJ, iran = $TUN_IP_IRAN (MTU $MTU)."
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse datagram tun (carrier "dgtun", encap $ENCAP from the link): kharej
    # DIALS the iran edge. No cert/domain. Kharej is the exit/panel side: every
    # user port the iran edge opens arrives on the tunnel's forwarder port and is
    # handed to the panel (expose). The user ports were asked on iran, and the
    # ipx number comes with the link — neither is asked again here.
    [ "$ENCAP" = ipx ] && ipx_proto_from_link
    ask_tun_params
    ask_panel "Panel inbound address on this server (iran's user ports are forwarded here) [127.0.0.1:8443]: "
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$ENCAP")
    write_cfg_checked <<EOF
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
    dialer_done "KHAREJ ready (reverse, tun / datagram pool over $ENCAP). It dials in to the Iran edge and forwards to $PANEL."
    info "Datagram L3 tunnel on $TUNIF once up: this kharej = $TUN_IP_KHAREJ, iran = $TUN_IP_IRAN (encap $ENCAP)."
  else
    # udp/auto: TUN IP tunnel on $TUNIF, no panel forwarding here.
    write_cfg_checked <<EOF
{
  "mode": "listen", "carrier": "$CARRIER", "reverse": true,
  "addr": "$ENDPOINT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_KHAREJ", "peer_ip": "$TUN_PEER_KHAREJ", "mtu": 1280,
  "shared_key": "$SHARED", "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service kharej; start_service kharej
    echo >&2; hr
    dialer_done "KHAREJ ready (reverse, $TRANSPORT / UDP+FEC). It dials in to the Iran edge."
    info "An IP tunnel is up on $TUNIF (kharej $TUN_IP_KHAREJ, iran $TUN_IP_IRAN). Route panel traffic over $TUNIF."
  fi
  info "Backhaul is untouched. Status/logs any time:  bash install.sh → 4"
  tunnel_summary
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
    ask_service_name     # this side makes the link, so it names the tunnel
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
  adopt_service_name; check_link_subnet; TUNIF=$(free_iface)
  ok "Link OK — kharej endpoint $ENDPOINT, transport $TRANSPORT (carrier $CARRIER)."
  ask_egress_ip
  mkdir -p "$(dirname "$CFG")"
  if [ "$TRANSPORT" = "tcp" ]; then
    ask_user_ip
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443): " PORTS </dev/tty
    [ -n "$PORTS" ] || die "at least one port is required"
    for p in ${PORTS//,/ }; do
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    write_cfg_checked <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": false, "udp": $UDP,
  "addr": "$ENDPOINT", "sni": "$DOMAIN",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
  "shared_key": "$SHARED",
  "forward_ports": "$PORTS", "peer_panel": "$PANEL",
  "min_links": $LINK_MIN, "max_links": $LINK_MAX, "per_link": $LINK_PER,
  "bind_local_ip": "$EGRESSIP", "user_listen_ip": "$USERIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    dialer_done "IRAN ready (direct, tcp). Users connect on port(s): $PORTS"
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
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    write_cfg_checked <<EOF
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
    dialer_done "IRAN ready (direct, tun over TLS: $(tun_tls_label))."
    info "L3 tunnel on $TUNIF once up: this iran = $TUN_IP_IRAN, kharej = $TUN_IP_KHAREJ (MTU $MTU)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel."
    else warn "No user ports: pure routed tun. Users can NOT reach the panel through this server unless you route traffic toward $TUN_IP_KHAREJ yourself."; fi
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
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      port_free "$p" || die "port $p is already in use on Iran (Backhaul or panel?). Pick another."
    done
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$ENCAP")
    write_cfg_checked <<EOF
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
    dialer_done "IRAN ready (direct, tun / datagram pool over $ENCAP)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel."
    else info "Pure routed L3 tunnel on $TUNIF: this iran = $TUN_IP_IRAN, kharej = $TUN_IP_KHAREJ. Route traffic toward $TUN_IP_KHAREJ."; fi
  else
    # udp/auto is a TUN IP tunnel on $TUNIF (not a port forwarder); no user ports.
    write_cfg_checked <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": false,
  "addr": "$ENDPOINT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED", "bind_local_ip": "$EGRESSIP"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    echo >&2; hr
    dialer_done "IRAN ready (direct, $TRANSPORT / UDP+FEC)."
    info "An IP tunnel is up on $TUNIF (iran $TUN_IP_IRAN, kharej $TUN_IP_KHAREJ)."
    warn "$TRANSPORT is an IP tunnel only: no user port listens here, so users can NOT reach the panel through this server unless you route traffic over $TUNIF yourself. For a panel inbound choose tcp, or tun (udp or tcp)."
    [ "$TRANSPORT" = "auto" ] && info "auto: if UDP is blocked or too lossy, it falls back to TCP silently."
  fi
  info "Backhaul is untouched. Status/logs any time:  bash install.sh → 4"
  tunnel_summary
}

# iran_listener: reverse edge. Iran listens for the kharej (which dials in) and
# generates the link. For tcp it also opens the user ports; for udp/auto it is a
# TUN IP tunnel on hs0.
iran_listener(){
  ask_subnet; TUNIF=$(free_iface)
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
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    ask_domain "Domain for THIS iran server (its A record must point to $PUBIP): "
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
    write_cfg_checked <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true, "udp": $UDP,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1380,
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
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    ask_domain "Domain for THIS iran server (its A record must point to $PUBIP): "
    ask_tun_params; ask_tun_mtu; LMTU="$TUNMTU"; tun_tls_carrier
    UDP=false
    if [ -n "$PORTS" ]; then
      read -rp "Also forward UDP on the user ports? [y/N]: " U </dev/tty
      case "$U" in y|Y|yes) UDP=true ;; *) UDP=false ;; esac
    fi
    local certpair CERT KEY
    certpair=$(get_cert "$DOMAIN" "$PUBIP"); CERT=${certpair%%|*}; KEY=${certpair##*|}
    write_cfg_checked <<EOF
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
    info "L3 tunnel on $TUNIF once up: this iran = $TUN_IP_IRAN, kharej = $TUN_IP_KHAREJ (MTU $LMTU)."
    if [ -n "$PORTS" ]; then info "Users connect on port(s): $PORTS — forwarded to the kharej panel (asked on the kharej)."
    else warn "No user ports: pure routed tun. Users can NOT reach the panel through this server unless you route traffic toward $TUN_IP_KHAREJ yourself."; fi
  elif [ "$TRANSPORT" = "tun" ]; then
    # Reverse datagram tun (carrier "dgtun", encap $TUN_ENCAP): iran LISTENS
    # (kharej dials in). No cert, no domain — shared-key auth only. Iran is the
    # edge, so it opens the user ports (forward_ports) and rides them over the pool.
    [ "$TUN_ENCAP" != "udp" ] || udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    ask_tun_params
    ask_user_ip
    read -rp "User port(s) to open here, comma-separated (e.g. 8443,443; Enter = none, pure routed tun): " PORTS </dev/tty
    for p in ${PORTS//,/ }; do
      valid_uint "$p" 1 65535 || die "user port '$p' must be a whole number 1-65535"
      [ "$p" != "$TPORT" ] || die "user port $p is the tunnel port — pick another."
      port_free "$p" || die "user port $p is already in use on Iran. Pick another."
    done
    LMTU=1280; CARRIER=dgtun; DOMAIN="-"; UDP=false
    info "Tunnel MTU is fixed at 1280 here — it leaves room for the datagram path's obfuscation and FEC overhead and is the same on both ends automatically (tun-over-TCP asks for an MTU; the datagram carriers do not)."
    local PROTOLINE; PROTOLINE=$(dgtun_proto_line "$TUN_ENCAP")
    write_cfg_checked <<EOF
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
    else info "Pure routed L3 tunnel on $TUNIF: this iran = $TUN_IP_IRAN, kharej = $TUN_IP_KHAREJ."; fi
  else
    udp_port_free "$TPORT" || die "UDP port $TPORT is already in use — pick another."
    [ "$TRANSPORT" = "auto" ] && { port_free "$TPORT" || die "auto also needs TCP port $TPORT free — pick another."; }
    CARRIER=$(transport_to_carrier "$TRANSPORT"); DOMAIN="-"; UDP=false
    write_cfg_checked <<EOF
{
  "mode": "dial", "carrier": "$CARRIER", "reverse": true,
  "addr": "$BINDADDR:$TPORT",
  "iface": "$TUNIF", "local_cidr": "$TUN_SUBNET_IRAN", "peer_ip": "$TUN_PEER_IRAN", "mtu": 1280,
  "shared_key": "$SHARED"
}
EOF
    chmod 600 "$CFG"; write_service iran; start_service iran
    ok "IRAN side is running (reverse, $TRANSPORT / UDP+FEC) and waits for the Kharej server to dial in."
    info "An IP tunnel is up on $TUNIF (iran $TUN_IP_IRAN, kharej $TUN_IP_KHAREJ)."
    warn "$TRANSPORT is an IP tunnel only: no user port listens here, so users can NOT reach the panel through this server unless you route traffic over $TUNIF yourself. For a panel inbound choose tcp, or tun (udp or tcp)."
  fi
  # panel is set on the kharej side; leave it blank in the link. The ipx number
  # goes in the link so the kharej uses the same one without asking.
  local ENCAP_ARG="" PROTO_ARG=""; [ "$TRANSPORT" = "tun" ] && ENCAP_ARG="$TUN_ENCAP"
  [ "$ENCAP_ARG" = ipx ] && PROTO_ARG="${TUN_PROTO:-253}"
  show_link "$PUBIP$PSUF" "$DOMAIN" "$SHARED" "-" "$CARRIER" "$UDP" "$TRANSPORT" "reverse" "$LMTU" "$ENCAP_ARG" "$PROTO_ARG"
  info "On the Kharej server: bash install.sh → 1 (Kharej) → direction 'reverse' → paste the link."
  info "The tunnel is tested end-to-end there: that side only says ready once packets really cross."
  info "It runs there as service $UNIT too (the name is in the link)."
  info "Then check this side any time:  hs2-menu → 3) Tunnel manager → $UNIT  — it shows ✓ connected once the other server is up."
  tunnel_summary
}

# tunnel_summary: the facts to keep about the tunnel just set up.
tunnel_summary(){
  local ifc lc peer car
  ifc=$(jget "$CFG" iface); lc=$(jget "$CFG" local_cidr); peer=$(jget "$CFG" peer_ip); car=$(jget "$CFG" carrier)
  echo >&2; hr
  say " Service:  ${C_B}$UNIT${C_0}   (systemctl status $UNIT · journalctl -u $UNIT -f)"
  say " Config:   $CFG"
  if [ "$car" != mtcp ] && [ -n "$ifc" ]; then say " Tun:      $ifc  ${lc%/*} ↔ $peer"; fi
  say " Manage:   hs2-menu → 3) Tunnel manager → $UNIT"
  hr
}

# ---------- uninstall & status ----------------------------------------------
# remove_tunnel UNIT: take one tunnel off this server — stop and disable its
# service, stop a straggler of THAT tunnel only, delete its tun interface (when
# no other tunnel's config names it), and remove its unit, config (+ .prev) and
# live status file. Other tunnels are not touched. Callers back up first.
remove_tunnel(){ # unit
  local u="$1" cfg pid ifc other c
  cfg=$(tm_cfg "$u"); [ -n "$cfg" ] || cfg=$(unit_cfg "$u")
  pid=$(tm_prop "$u" MainPID)
  systemctl disable --now "$u" >/dev/null 2>&1 || true
  sleep 1
  # Never `pkill -x hs2`: the other tunnels run the same binary.
  kill_this_tunnel TERM "$pid" "$cfg"; sleep 1; kill_this_tunnel KILL "$pid" "$cfg"
  ifc=$(cfg_tun_iface "$cfg")
  if [ -n "$ifc" ]; then
    other=""
    for c in $(tm_units); do
      [ "$c" = "$u" ] && continue
      [ "$(cfg_tun_iface "$(tm_cfg "$c")")" = "$ifc" ] && other=$c
    done
    if [ -z "$other" ]; then ip link del "$ifc" 2>/dev/null || true; fi
  fi
  rm -f "$UNIT_DIR/$u.service" "$cfg" "$cfg.prev" "$cfg.pre-replace" "$(status_path "$cfg")"
  systemctl daemon-reload 2>/dev/null || true
  systemctl reset-failed "$u" >/dev/null 2>&1 || true
  kernel_cleanup
}

# kernel_cleanup: remove what stopped tunnels left in the kernel — an icmp
# tunnel's reply rule (nft/iptables) whose daemon is gone, or ping replies a
# dead daemon turned off. Rules of running tunnels are never touched. (An older
# binary has no `cleanup` command; then there is nothing to do.)
kernel_cleanup(){
  local out ln
  out=$("$BIN" cleanup 2>/dev/null) || return 0
  while IFS= read -r ln; do
    case "$ln" in removed:*) info "Cleaned up ${ln#removed: }" ;; esac
  done <<<"$out"
  return 0
}

# uninstall: remove EVERY hs2 tunnel from this server (the tunnel manager
# deletes one at a time). The binary and hs2-menu stay, so setting up again
# needs no download.
uninstall(){
  local a u units
  units=$(tm_units)
  if [ -z "$units" ]; then info "No hs2 tunnel on this server — nothing to remove."; return 0; fi
  echo >&2; warn "This removes ALL hs2 tunnels on this server:"
  for u in $units; do say "     $u — $(unit_desc "$u")"; done
  info "(To remove just one, use the Tunnel manager → the tunnel → Delete.)"
  # A whole-server wipe deserves a deliberate confirmation, not a single 'y' —
  # the same discipline deleting ONE tunnel already requires (typing its name).
  # A backup is saved first either way.
  read -rp "Type REMOVE ALL to wipe every tunnel (a backup is saved first), or Enter to cancel: " a </dev/tty || a=""
  case "$a" in "REMOVE ALL") ;; *) info "Nothing removed."; return 0 ;; esac
  auto_backup
  for u in $units; do info "Removing $u…"; remove_tunnel "$u"; done
  rm -f /etc/sysctl.d/99-hs2.conf /etc/modules-load.d/hs2.conf
  ok "All hs2 tunnels removed (services stopped, tun interfaces deleted). Backhaul untouched."
  if ls "$BACKUP_DIR"/hs2-*.tar.gz >/dev/null 2>&1; then
    warn "Backups are kept in $BACKUP_DIR (readable by root only). They contain the tunnel keys —"
    warn "delete that folder if this server is being handed over or the tunnels are gone for good."
  fi
}

# kill_this_tunnel SIGNAL [PID] [CFG]: signal one tunnel's hs2 process — PID
# when it is still an hs2 process, and any `hs2 run -c CFG` — and nothing else.
kill_this_tunnel(){ # signal [pid] [cfg]
  local sig="$1" pid="${2:-}" cfg="${3:-$CFG}" p args
  if [ -n "$pid" ] && [ "$pid" != 0 ] && [ -r "/proc/$pid/cmdline" ]; then
    args=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)
    case "$args" in *hs2*) kill -"$sig" "$pid" 2>/dev/null || true ;; esac
  fi
  for p in $(pgrep -x hs2 2>/dev/null || true); do
    args=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null || true)
    case "$args" in *" -c $cfg "*) kill -"$sig" "$p" 2>/dev/null || true ;; esac
  done
  return 0
}

# status: every tunnel — service state, its tun interface, and its recent log.
status(){
  local u cfg ifc units
  units=$(tm_units)
  if [ -z "$units" ]; then warn "No hs2 tunnel on this server yet."; return 0; fi
  for u in $units; do
    cfg=$(tm_cfg "$u")
    hr; say " ${C_B}$u${C_0} — $(unit_desc "$u")"; hr
    say " Service:     $(tm_state_label "$(tm_state "$u")")"
    if [ "$(tm_state "$u")" = running ]; then say " Connection:  $(tunnel_conn_label "$cfg")"; fi
    systemctl status "$u" --no-pager 2>/dev/null | head -8 >&2 || true
    ifc=$(jget "$cfg" iface)
    if [ "$(jget "$cfg" carrier)" != mtcp ] && [ -n "$ifc" ]; then
      if ip -br addr show "$ifc" >/dev/null 2>&1; then ok "Tunnel interface $ifc: $(ip -br addr show "$ifc" | awk '{print $3}')"
      else warn "Tunnel interface $ifc is not up."; fi
    fi
    info "Recent log of $u:"
    journalctl -u "$u" -n 12 --no-pager -o cat 2>/dev/null | sed 's/^/     /' >&2 || true
    echo >&2
  done
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

# Role reads, hardened. jget/jraw grab the FIRST "<key>": match anywhere in the
# file, so on a HAND-EDITED config `jget mode` can return the nested
# tuning.mode value (auto/manual/off) instead of the top-level dial/listen, and
# `jraw reverse` misses an upper-case True. The role decides which physical
# server a tunnel is, so a misread is serious. These test the VALUE directly and
# are immune to key order and to tuning.mode (which is never "dial"); it is the
# same value-anchored idiom upgrade()/write_service already use. (A real JSON
# read would need the binary, but `hs2 config get` exposes none of these keys.)
cfg_is_dial(){ grep -Eq '"mode"[[:space:]]*:[[:space:]]*"dial"' "$1" 2>/dev/null; }
cfg_is_reverse(){ grep -Eiq '"reverse"[[:space:]]*:[[:space:]]*true' "$1" 2>/dev/null; }

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

# tm_units lists every tunnel (hs2.service first, then hs2-*.service by name).
# A MASKED unit is a /dev/null symlink, not a tunnel file — it is listed too, so
# setting one up again (which unmasks it) and the manager can see it.
tm_units(){
  local f
  for f in "$UNIT_DIR/hs2.service" "$UNIT_DIR"/hs2-*.service; do
    { [ -f "$f" ] || [ -L "$f" ]; } && basename "$f" .service
  done
  return 0
}
tm_cfg(){ { sed -n 's/^ExecStart=.* -c \([^ ]*\).*/\1/p' "$UNIT_DIR/$1.service" 2>/dev/null || true; } | head -1; }
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
  if cfg_is_dial "$1"; then ! cfg_is_reverse "$1"; else cfg_is_reverse "$1"; fi
}
# Live TLS links of a TCP tunnel. Prefer the daemon's own live count (it knows
# exactly how many links are up, and its dynamic target); fall back to counting
# established sockets when no fresh status file is there (older binary).
tm_links(){
  local cfg="$1" car addr host port sf
  car=$(jget "$cfg" carrier)
  sf=$(status_path "$cfg")
  if status_fresh "$sf"; then
    jraw "$sf" links; return 0
  fi
  case "$car" in mtcp|l3mtcp|l3|tls) ;; *) return 0 ;; esac
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
tm_role(){ if cfg_is_dial "$1"; then echo "Iran side"; else echo "Kharej side"; fi; }
tm_dir(){ if cfg_is_reverse "$1"; then echo reverse; else echo direct; fi; }

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

# tm_peer_ok UNIT CFG: 0 if the peer is reachable, 1 if not — cached ~15 s per
# unit so the manager list does not re-run a (2-4 s when down) probe on every
# redraw. The probe (tunnel_up … list) is loss- and latency-tolerant; the cache
# is cleared on an explicit Refresh and after any tunnel action.
declare -A TM_PEER_CACHE=()
tm_peer_ok(){ # unit cfg
  local u="$1" cfg="$2" now ts res
  now=$(date +%s)
  if [ -n "${TM_PEER_CACHE[$u]:-}" ]; then
    ts=${TM_PEER_CACHE[$u]%%:*}; res=${TM_PEER_CACHE[$u]##*:}
    [ $((now - ts)) -lt 15 ] && return "$res"
  fi
  if tunnel_up "$cfg" list; then res=0; else res=1; fi
  TM_PEER_CACHE[$u]="$now:$res"
  return "$res"
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
    # Real peer reachability, not just "the process is alive": a running service
    # whose peer is gone (the other side deleted, down, or the path closed) would
    # otherwise show green here while carrying nothing. tunnel_up actually proves
    # the far end answers, so flag it plainly on the first screen the operator sees.
    if [ "$st" = running ] && [ -f "$cfg" ] && ! tm_peer_ok "$u" "$cfg"; then
      extra="$extra · ${C_R}✗ no peer${C_0}"
    fi
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
# tm_health_lines SF: loss / FEC / policer / drops / CPU from a fresh status
# file (datagram tunnels report loss of what THIS side sends, as its peer sees it).
tm_health_lines(){ # status file
  local sf="$1" car loss mloss par ceil rec lost pol pconf pcap pd rd td cpu cores tr sp rp tw dn dq da
  status_fresh "$sf" || return 0
  car=$(jget "$sf" carrier); pol=$(jraw "$sf" policed); pconf=$(jraw "$sf" police_confirmed)
  loss=$(jraw "$sf" loss_pct); mloss=$(jraw "$sf" max_loss_pct); par=$(jraw "$sf" parity_pct)
  ceil=$(jraw "$sf" fec_at_ceiling); rec=$(jraw "$sf" fec_recovered); lost=$(jraw "$sf" fec_lost)
  pcap=$(jraw "$sf" police_cap_mbit); pd=$(jraw "$sf" pacer_dropped); rd=$(jraw "$sf" rx_dropped); td=$(jraw "$sf" tun_drops)
  cpu=$(jraw "$sf" cpu_pct); cores=$(jraw "$sf" cpu_cores)
  tr=$(jraw "$sf" tun_read); sp=$(jraw "$sf" sent_pkts); rp=$(jraw "$sf" recv_pkts); tw=$(jraw "$sf" tun_written)
  dn=$(jraw "$sf" drop_no_carrier); dq=$(jraw "$sf" drop_queue_full); da=$(jraw "$sf" drop_aged)
  case "$car" in dgtun*)
    say " Loss:        ${loss:-0}% of what this side sends (worst carrier ${mloss:-0}%)"
    say " FEC:         parity ${par:-0}% of data$([ -n "$ceil" ] && echo " · ${C_Y}at its ceiling on $ceil carrier(s)${C_0}") · received ${rec:-0} rebuilt, ${lost:-0} lost"
    if [ "$pol" = true ] && [ "$pconf" = true ]; then say " Policer:     ${C_Y}confirmed on the path${C_0} — whole pool held at ${pcap} Mbit/s (re-probes slowly)"
    elif [ "$pol" = true ]; then say " Policer:     suspected — testing with the whole pool capped at ${pcap} Mbit/s"; fi
    say " Packets:     tun→carriers ${tr:-0} read, ${sp:-0} sent · carriers→tun ${rp:-0} received, ${tw:-0} written"
    [ -n "$pd$rd$td" ] && say " Drops:       no carrier ${dn:-0} · carrier queue full ${dq:-0} · waited >50 ms ${da:-0} · pacer ${pd:-0} · receive queue ${rd:-0}"
    ;;
  esac
  [ -n "$cores" ] && say " CPU:         ${cpu:-0}% of one core ($cores core(s))"
  return 0
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
  if [ "$st" = running ] && [ -f "$cfg" ]; then say " Connection:  $(tunnel_conn_label "$cfg")"; fi
  if [ -f "$cfg" ]; then
    say " Side:        $(tm_role "$cfg") · $(tm_dir "$cfg") · $(tm_transport "$cfg")"
    if tm_dials "$cfg"; then
      bind=$(jget "$cfg" bind_local_ip)
      say " Connection:  connects to $(jget "$cfg" addr) from ${bind:-the default IP}"
    else
      say " Connection:  listens on $(jget "$cfg" addr)$([ "$st" = running ] && [ -n "$(tm_peers "$cfg")" ] && echo " · connected: $(tm_peers "$cfg")")"
    fi
    if [ "$(jget "$cfg" carrier)" != mtcp ] && [ -n "$(jget "$cfg" iface)" ]; then
      say " Tun:         $(jget "$cfg" iface)  $(jget "$cfg" local_cidr | sed 's#/.*##') ↔ $(jget "$cfg" peer_ip)"
    fi
    [ -n "$(jget "$cfg" forward_ports)" ] && say " User ports:  $(jget "$cfg" forward_ports)  (users connect here)"
    [ -n "$(jget "$cfg" expose)" ] && say " Panel:       $(jget "$cfg" expose)"
  fi
  if [ "$st" = running ]; then
    local pat sf cd
    pat=$(tm_pattern "$cfg")
    [ -n "$pat" ] && say " Pattern:     $pat"
    sf=$(status_path "$cfg")
    tm_health_lines "$sf"
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
      tm_health_lines "$sf"
    else
      say " Waiting for live status… (needs the new binary; older tunnels show links only)"
      local links; links=$(tm_links "$cfg")
      [ -n "$links" ] && say " Links:   ${links} (from open sockets)"
    fi
    hr
    say " ${C_D}refreshing every 2s · press Enter to go back${C_0}"
    # The -t 2 read is the refresh clock. With no controlling terminal the
    # </dev/tty redirect fails INSTANTLY (no 2 s wait), busy-looping and pegging a
    # core — so pace with sleep instead. Test by actually OPENING /dev/tty, not
    # with [ -r ]: the device node is mode-readable even when it cannot be opened.
    if { : </dev/tty; } 2>/dev/null; then
      read -rp "" -t 2 _ </dev/tty && break || true
    else
      sleep 2
    fi
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
# tm_bilateral_changed OLDCFG NEWCFG — prints the names of any config fields
# that MUST be identical on the OTHER server (they travel in the hs2:// link or
# decide authentication) and that the edit changed. tm_validate only checks the
# LOCAL file, so a one-sided change to one of these passes validation and
# restarts cleanly here yet silently breaks the tunnel — this surfaces it.
tm_bilateral_changed(){ # oldcfg newcfg
  local spec key getter old new ob nb
  # Fields that must be BYTE-IDENTICAL on both ends (they travel in the link or
  # decide authentication). local_cidr and addr are deliberately NOT here: each
  # side's tun host differs by design (iran base+1, kharej base+2), and addr is
  # a per-side bind/endpoint — telling the operator to copy them to the peer
  # would create a duplicate tun IP or a wrong endpoint.
  for spec in shared_key:jget carrier:jget encap:jget sni:jget mtu:jraw proto:jraw reverse:jraw; do
    key=${spec%:*}; getter=${spec#*:}
    old=$("$getter" "$1" "$key"); new=$("$getter" "$2" "$key")
    [ "$old" = "$new" ] || echo "$key"
  done
  # The /30 BASE is bilateral (it is what travels in the link); the per-side host
  # inside it is not, so compare the base rather than the local_cidr string. Both
  # configs already passed `hs2 check`, so local_cidr is a valid CIDR here.
  old=$(jget "$1" local_cidr); new=$(jget "$2" local_cidr)
  if [ -n "$old" ] && [ -n "$new" ]; then
    ob=$(block_of "${old%/*}"); nb=$(block_of "${new%/*}")
    [ "$ob" = "$nb" ] || echo "tunnel subnet"
  fi
}

# tm_bilateral_warn CHANGED — warn about bilateral fields the edit changed and
# give the operator the safe remediation. CHANGED is the newline list from
# tm_bilateral_changed; empty => nothing to say.
tm_bilateral_warn(){ # changed-list
  [ -n "$1" ] || return 0
  local k
  echo >&2; hr
  warn "You changed setting(s) that MUST be the SAME on the OTHER server:"
  for k in $1; do say "     • $k"; done
  warn "Until the other side matches, this tunnel will not come up or will not authenticate —"
  warn "the check above only validates THIS server's file, not the pair."
  info "Fix the other server: edit its config to the same value(s), or re-run 'bash install.sh'"
  info "on the LINK-MAKING side to generate a fresh link and paste it there."
  hr
}

tm_edit(){
  local u="$1" cfg="$2" ed tmp c a since changed
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
  # Capture which bilateral (must-match-the-peer) fields the edit changed, while
  # $cfg is still the OLD file and $tmp holds the NEW one (before the swap).
  changed=$(tm_bilateral_changed "$cfg" "$tmp")
  cat "$tmp" > "$cfg"; rm -f "$tmp"
  ok "Saved. The previous version is kept as $cfg.prev"
  since=$(date '+%Y-%m-%d %H:%M:%S')
  info "Restarting $u to apply the change…"
  systemctl restart "$u" 2>/dev/null || true
  if tm_healthy "$u"; then
    ok "Applied — $u is running with the new config."
    tm_log_since "$u" "$since"
    tm_bilateral_warn "$changed"
    return 0
  fi
  err "$u did not come up with the new config. Log:"
  tm_log_since "$u" "$since"
  read -rp "Put the previous config back and restart? [Y/n]: " a </dev/tty || a=y
  case "${a:-y}" in
    n|N|no) warn "Left the new config in place. Fix it with Edit, or restore $cfg.prev."
            tm_bilateral_warn "$changed" ;;
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
  local u="$1" cfg="$2" c v out
  # Capture first, THEN grep: piping `"$BIN" tune` straight into `grep -q` lets
  # pipefail return the binary's own non-zero (an old binary exits non-zero on
  # the unknown subcommand) even though grep matched, so the old-binary guard was
  # skipped and the menu fell through to a tune loop that cannot work.
  out=$("$BIN" tune -c "$cfg" 2>&1) || true
  if printf '%s' "$out" | grep -q "unknown command"; then
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
    say "  6) Link pool (advanced) — min / max links and users-per-link step"
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
      6) tm_tune_links "$u" "$cfg" ;;
      0|b|B|"") return 0 ;;
      *) warn "Invalid choice." ;;
    esac
  done
}

# tm_tune_links: adjust the adaptive parallel-link pool — the min/max link count
# and the users-per-link growth step. The pool still sizes itself live between
# min and max; this only moves the envelope (previously only editable by hand in
# the JSON — U7). These are config keys the binary itself validates; an empty
# answer keeps the current value, and all three are applied as a unit with a
# rollback to .prev if any fails, like manual tuning.
tm_tune_links(){ # unit cfg
  local u="$1" cfg="$2" cur_min cur_max cur_per mn mx pl v
  cur_min=$(jraw "$cfg" min_links); cur_max=$(jraw "$cfg" max_links); cur_per=$(jraw "$cfg" per_link)
  echo >&2
  say "  Adaptive link pool — hs2 grows and shrinks links live between min and max."
  say "  Current: min=${cur_min:-2} · max=${cur_max:-32} · ~${cur_per:-8} users per link"
  read -rp "  Min links [${cur_min:-2}]: " mn </dev/tty;         mn=${mn:-${cur_min:-2}}
  read -rp "  Max links [${cur_max:-32}]: " mx </dev/tty;        mx=${mx:-${cur_max:-32}}
  read -rp "  Users per link [${cur_per:-8}]: " pl </dev/tty;    pl=${pl:-${cur_per:-8}}
  for v in "$mn" "$mx" "$pl"; do
    valid_uint "$v" 1 1024 || { warn "Each value must be a whole number 1-1024."; return 0; }
  done
  [ "$mn" -le "$mx" ] || { warn "Min links ($mn) cannot be more than max links ($mx)."; return 0; }
  cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
  # The binary re-validates min<=max after EACH set, so the order matters: when
  # the new floor is above the current ceiling, raise max FIRST (else `set
  # min_links` is rejected against the old smaller max); otherwise set min first
  # (so lowering max below the old min is not rejected either). The guard above
  # guarantees mn<=mx, so one of the two orders is always valid end-to-end.
  local ok=1
  if [ "$mn" -gt "${cur_max:-32}" ]; then
    tm_cfgset "$cfg" max_links "$mx" && tm_cfgset "$cfg" min_links "$mn" && tm_cfgset "$cfg" per_link "$pl" || ok=0
  else
    tm_cfgset "$cfg" min_links "$mn" && tm_cfgset "$cfg" max_links "$mx" && tm_cfgset "$cfg" per_link "$pl" || ok=0
  fi
  if [ "$ok" != 1 ]; then
    warn "Could not apply all values — restoring the previous config."
    [ -f "$cfg.prev" ] && cat "$cfg.prev" > "$cfg"
    return 0
  fi
  tm_apply_restart "$u" "$cfg"
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
       r=$((10#$m*1024*1024)); w=$r; b=8192; s=4096 ;;
    0|b|B|"") return 0 ;;
    *) warn "Invalid choice."; return 0 ;;
  esac
  cp -p "$cfg" "$cfg.prev" 2>/dev/null || true
  # Apply all five as one unit: if any set fails partway, roll the config back to
  # .prev instead of leaving it half-tuned (a mix of old and new values).
  if ! { tm_cfgset "$cfg" tuning.mode manual \
      && tm_cfgset "$cfg" tuning.rmem_max "$r" \
      && tm_cfgset "$cfg" tuning.wmem_max "$w" \
      && tm_cfgset "$cfg" tuning.netdev_backlog "$b" \
      && tm_cfgset "$cfg" tuning.somaxconn "$s"; }; then
    warn "Could not apply all tuning values — restoring the previous config."
    [ -f "$cfg.prev" ] && cat "$cfg.prev" > "$cfg"
    return 0
  fi
  tm_apply_restart "$u" "$cfg"
}

# tm_delete UNIT: remove one tunnel from this server after the operator types
# its name (a destructive step deserves more than a y). Everything is backed up
# first; the other tunnels keep running untouched. 0 = deleted.
tm_delete(){ # unit
  local u="$1" a v
  echo >&2; hr
  warn "Delete ${u}: its service, config and tun interface are removed from this server."
  say "     $u — $(unit_desc "$u")"
  info "The other tunnels keep running untouched. A backup is saved first (restore: bash install.sh restore)."
  info "The other server keeps its side of this tunnel — delete it there too if the tunnel is gone for good."
  hr
  read -rp "Type the service name to delete it ($u), or Enter to cancel: " a </dev/tty || a=""
  v=$(norm_unit "$a" 2>/dev/null || true)
  if [ -z "$a" ] || [ "$v" != "$u" ]; then info "Not deleted."; return 1; fi
  auto_backup
  info "Deleting $u…"
  remove_tunnel "$u"
  if unit_exists "$u"; then err "$u could not be removed completely — check $UNIT_DIR/$u.service"; return 1; fi
  ok "$u deleted."
  return 0
}

# tm_doctor runs `hs2 doctor` for one tunnel: a read-only health check (config,
# endpoint reachability, certificate, TUN device, kernel tuning, clock). Like
# tm_tune it captures first, THEN greps for the old-binary marker — piping
# straight into grep -q would let pipefail surface the binary's own non-zero
# (doctor exits 1 when it finds a problem) and mis-fire the guard. `|| true`
# keeps the capture safe under set -e even on that exit-1.
tm_doctor(){ # unit cfg
  local u="$1" cfg="$2" out
  out=$("$BIN" doctor -c "$cfg" 2>&1) || true
  if printf '%s' "$out" | grep -q "unknown command"; then
    warn "This hs2 binary is too old for 'doctor'. Upgrade first (menu → 5, or 'u' here)."; pause; return 0
  fi
  echo >&2; hr; say " ${C_B}Diagnose${C_0} — $u"; hr
  printf '%s\n' "$out" | sed 's/^/  /' >&2
  hr
  pause
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
    say "  d) Diagnose (health check — config, endpoint, certificate, tuning)"
    say "  u) Upgrade THIS tunnel to the latest binary (restarts only this one)"
    say "  9) Delete this tunnel (service, config and tun interface)"
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
      d|D) tm_doctor "$u" "$cfg" ;;
      u|U) upgrade "$u"; pause ;;
      9) if tm_delete "$u"; then pause; return 0; fi ;;
      0|b|B|"") return 0 ;;
      *) warn "Invalid choice." ;;
    esac
  done
}

tunnel_manager(){
  local c
  TM_PEER_CACHE=()          # fresh peer probe on entry
  while :; do
    tm_list
    [ ${#TM_UNITS[@]} -gt 0 ] || { pause; return 0; }
    say ""; say "  Pick a tunnel number to manage it · r) Refresh · 0) Back"
    read -rp "Choose: " c </dev/tty || return 0
    case "$c" in
      0|b|B|q) return 0 ;;
      r|R|"") TM_PEER_CACHE=(); continue ;;          # Refresh: re-probe peers
      *[!0-9]*) warn "Invalid choice." ;;
      *) if [ "$c" -ge 1 ] && [ "$c" -le ${#TM_UNITS[@]} ]; then
           tm_tunnel_menu "${TM_UNITS[$((10#$c-1))]}"; TM_PEER_CACHE=()   # a tunnel action may change peer state
         else warn "There is no tunnel $c."; fi ;;
    esac
  done
}

# Keep a copy of this script as the `hs2-menu` command, so the menu works even
# when GitHub is unreachable (Iran side).
install_self(){
  local tmp; tmp=$(mktemp)
  # Prefer the script that is actually running (a local file we already trust),
  # so there is no network fetch and no window to swap the root-run hs2-menu.
  if [ -f "$0" ] && grep -q 'hs2 v3' "$0" 2>/dev/null; then
    cp "$0" "$tmp"
  elif curl -fsSL --connect-timeout 10 -o "$tmp" "$REPO_RAW/install.sh" 2>/dev/null && grep -q 'hs2 v3' "$tmp"; then
    # Fetched over the network (curl|bash: no local file). hs2-menu runs as
    # root, so verify it against the published install.sh.sha256 and fail CLOSED
    # — the same discipline the binary gets.
    local sraw swant sgot
    sraw=$(curl -fsSL --connect-timeout 10 "$REPO_RAW/install.sh.sha256" 2>/dev/null || true)
    swant=$(printf '%s' "$sraw" | grep -oE '[0-9a-fA-F]{64}' | head -1 | tr 'A-F' 'a-f' || true)
    sgot=$(sha256sum "$tmp" | cut -d' ' -f1)
    if [ -z "$swant" ] || [ "$sgot" != "$swant" ]; then
      rm -f "$tmp"
      warn "install.sh could not be verified against install.sh.sha256 — hs2-menu not installed. Re-run 'bash install.sh' from a trusted copy to add the menu shortcut."
      return 0
    fi
  else
    rm -f "$tmp"; return 0
  fi
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
  local units u c p d q f meta items=()
  units=$(tm_units)
  [ -n "$units" ] || { warn "Nothing to back up (no hs2 tunnel on this server)."; return 0; }
  mkdir -p "$BACKUP_DIR"; chmod 700 "$BACKUP_DIR"
  f="$BACKUP_DIR/hs2-$(hostname -s 2>/dev/null || echo host)-$(date +%Y%m%d-%H%M%S).tar.gz"
  # Every tunnel: its unit, its config (+ the .prev the editor keeps) and the
  # certificate it uses — the whole letsencrypt lineage when it is a certbot
  # cert, so renewal keeps working after a restore.
  for u in $units; do
    [ -f "$UNIT_DIR/$u.service" ] && items+=("${UNIT_DIR#/}/$u.service")
    c=$(tm_cfg "$u")
    for p in "$c" "$c.prev"; do [ -n "$c" ] && [ -f "$p" ] && items+=("${p#/}"); done
    for p in "$(jget "$c" cert_file)" "$(jget "$c" key_file)"; do
      [ -n "$p" ] || continue
      case "$p" in
        /etc/letsencrypt/live/*)
          d=$(echo "$p" | cut -d/ -f5)
          for q in "/etc/letsencrypt/live/$d" "/etc/letsencrypt/archive/$d" "/etc/letsencrypt/renewal/$d.conf"; do
            [ -e "$q" ] && items+=("${q#/}")
          done ;;
        *) [ -e "$p" ] && items+=("${p#/}") ;;
      esac
    done
  done
  for p in "$BIN" /etc/sysctl.d/99-hs2.conf; do [ -e "$p" ] && items+=("${p#/}"); done
  [ ${#items[@]} -gt 0 ] || { warn "Nothing to back up."; return 0; }
  # Dedupe (cert and key share a lineage) and write a human-readable summary.
  mapfile -t items < <(printf '%s\n' "${items[@]}" | sort -u)
  meta=$(mktemp -d)
  {
    echo "hs2 backup $(date -Is) on $(hostname)"
    "$BIN" version 2>/dev/null || true
    for u in $units; do
      c=$(tm_cfg "$u")
      echo "$u: $(unit_desc "$u") · iface=$(jget "$c" iface) tun=$(jget "$c" local_cidr) · service $(systemctl is-active "$u" 2>/dev/null || true)"
    done
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
  local n=0 f old=()
  case "$BACKUP_KEEP" in ''|*[!0-9]*|0) return 0 ;; esac
  # mapfile (not `for f in $old`): read whole lines into an array so a backup
  # path with a space or glob char in it (an odd hostname) is never word-split
  # or glob-expanded.
  mapfile -t old < <(ls -1t "$BACKUP_DIR"/hs2-*.tar.gz 2>/dev/null | tail -n +$((BACKUP_KEEP + 1)))
  # Guard the empty case (the normal one: fewer than KEEP backups): expanding an
  # empty array as "${old[@]}" under set -u aborts on bash < 4.4 (CentOS 7 etc.),
  # and backup()/auto_backup run this before every setup. Same guard as tm_list.
  [ ${#old[@]} -gt 0 ] || return 0
  for f in "${old[@]}"; do [ -n "$f" ] && rm -f "$f" && n=$((n + 1)); done
  [ "$n" = 0 ] || info "Removed $n old backup(s); the newest $BACKUP_KEEP are kept in $BACKUP_DIR."
  return 0
}

# restore FILE: put back every tunnel in the backup, exactly as saved. Tunnels
# added after the backup was made are not in it and are left running as they
# are. A backup from before named tunnels holds just "hs2" — same path.
restore(){
  local f="${1:-}" list units u c ifc fails=0 ans=""
  if [ -z "$f" ]; then
    f=$(ls -1t "$BACKUP_DIR"/hs2-*.tar.gz 2>/dev/null | head -1)
    [ -n "$f" ] || die "no backups in $BACKUP_DIR"
    info "Backups (newest first):"; ls -1t "$BACKUP_DIR"/hs2-*.tar.gz | sed 's/^/   /' >&2
    # read -p prints its prompt on stderr, so stderr must stay visible here.
    if [ -r /dev/tty ]; then
      read -rp "Restore which file? (Enter = newest) [$f]: " ans </dev/tty || true
      f=${ans:-$f}
    fi
  fi
  [ -f "$f" ] || die "backup not found: $f"
  list=$(tar -tzf "$f" 2>/dev/null || true)
  # The tunnels in the backup = its hs2 unit files. (Plain sed, never grep -q:
  # an early exit would kill tar with SIGPIPE and pipefail would call a good
  # backup bad.)
  units=$(printf '%s\n' "$list" | sed -n 's#^.*systemd/system/\(hs2[a-z0-9-]*\)\.service$#\1#p' | sort -u)
  if [ -z "$units" ] && printf '%s\n' "$list" | grep -Fx "${CFG_DIR#/}/config.json" >/dev/null; then units=hs2; fi
  [ -n "$units" ] || die "$f is not an hs2 backup (no tunnel inside)."
  # `|| true`: an older backup has no hs2-backup-info.txt, so tar exits non-zero
  # and under pipefail+set -e that would abort the whole restore before it began.
  hr; info "Restoring $f"; { tar -xzOf "$f" hs2-backup-info.txt 2>/dev/null | sed 's/^/   /' >&2; } || true; hr
  info "Tunnels in this backup: $(echo $units)"
  for u in $(tm_units); do
    printf '%s\n' "$units" | grep -Fqx "$u" || info "$u is not in this backup — left as it is."
  done
  # Take the backed-up tunnels down (and their interfaces: the restored config
  # may use another name) before files are replaced; the safety net starts them
  # again if anything below fails.
  for u in $units; do
    unit_exists "$u" || continue
    HS2_STOPPED_UNITS="$HS2_STOPPED_UNITS $u"
    c=$(tm_cfg "$u"); ifc=$(cfg_tun_iface "$c")
    systemctl stop "$u" 2>/dev/null || true
    [ -n "$ifc" ] && ip link del "$ifc" 2>/dev/null || true
  done
  for u in $units; do unit_unmask "$u"; done   # else a restored unit file lands on a /dev/null symlink
  # Clean-break guard: this backup carries the hs2 binary from when it was made.
  # If OTHER tunnels (not in this backup) are on this server and that binary
  # differs from the one installed now, extracting it would DOWNGRADE the shared
  # binary under them and break their link (both ends must match) on the next
  # restart — so keep the installed binary in that case.
  local binex="" tmpbin other=0
  for u in $(tm_units); do printf '%s\n' "$units" | grep -Fqx "$u" || other=1; done
  if [ "$other" = 1 ] && printf '%s\n' "$list" | grep -Fqx "${BIN#/}"; then
    # mktemp must not be a bare command under set -e (a failure would abort the
    # whole restore); on failure skip the check and extract normally.
    tmpbin=$(mktemp 2>/dev/null) || tmpbin=""
    if [ -n "$tmpbin" ] && tar -xzOf "$f" "${BIN#/}" >"$tmpbin" 2>/dev/null && [ -s "$tmpbin" ] \
       && [ -x "$BIN" ] && ! cmp -s "$tmpbin" "$BIN"; then
      warn "This backup's hs2 binary differs from the installed one, and other tunnels here are not"
      warn "in this backup — keeping the installed binary so they are not downgraded (clean break)."
      binex="--exclude=${BIN#/}"
    fi
    [ -n "$tmpbin" ] && rm -f "$tmpbin"
  fi
  if [ -n "$binex" ]; then
    tar -xzf "$f" -C / --exclude=hs2-backup-info.txt "$binex" || die "extract failed"
  else
    tar -xzf "$f" -C / --exclude=hs2-backup-info.txt || die "extract failed"
  fi
  systemctl daemon-reload
  [ -f /etc/sysctl.d/99-hs2.conf ] && sysctl -p /etc/sysctl.d/99-hs2.conf >/dev/null 2>&1 || true
  for u in $units; do
    use_unit "$u"
    if [ ! -f "$SVC" ] && [ -f "$CFG" ]; then   # a very old backup without the unit
      if grep -q '"mode"[[:space:]]*:[[:space:]]*"dial"' "$CFG"; then write_service iran; else write_service kharej; fi
    fi
    systemctl enable "$u" >/dev/null 2>&1 || true
    if restart_unit "$u"; then ok "$u restored and running."
    else err "$u failed to start after restore. Last log:"; journalctl -u "$u" -n 20 --no-pager >&2 || true; fails=$((fails + 1)); fi
  done
  ok "Binary: $("$BIN" version 2>/dev/null)"
  [ "$fails" = 0 ] || bail
  ok "Autostart on boot: ON"
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
  [ -n "$(tm_units)" ] || return 0
  info "Existing hs2 tunnel(s) found — backing them up first (restore: bash install.sh restore)."
  backup >/dev/null
}

# Upgrade in place: new binary, and the chosen tunnel(s) restarted on it with
# their own config and hs2:// link unchanged (each is checked, and reconnects
# checked). With a tunnel name it upgrades only that one; with none, all of
# them. Either way the user sees the exact restart list and confirms first — a
# restart briefly drops that tunnel's traffic, which matters on a busy tunnel.
upgrade(){ # [tunnel]
  local all units u role fails=0 a
  all=$(tm_units)
  [ -n "$all" ] || die "hs2 is not installed on this server (no tunnel). Run without 'upgrade' to install."
  if [ -n "${1:-}" ]; then
    u=$(norm_unit "$1") || die "bad tunnel name: $1"
    unit_exists "$u" || die "no such tunnel on this server: $u (open the Tunnel manager to list them)."
    units="$u"
  else
    units="$all"
  fi
  hr; info "Upgrade hs2 — the latest binary, then RESTART these tunnel(s):"
  for u in $units; do say "     • $u — $(unit_desc "$u")"; done
  say "   A restart briefly drops that tunnel's traffic (a few seconds)."
  if [ "$units" != "$all" ]; then
    # The binary is one shared file; replacing it moves every tunnel to the new
    # build on ITS next restart. We only restart the chosen one now, but the
    # others are no longer pinned to the old build — say so plainly.
    warn "This replaces the shared binary. Other tunnels keep running the current"
    warn "build until they are restarted (a crash, a reboot, or their own upgrade)."
  fi
  # Unattended runs (CI, cron, curl|bash with no tty) set HS2_YES=1 to proceed;
  # otherwise a missing tty skips the read and the empty default CANCELS — a
  # restart is never kicked off without a yes.
  if [ "${HS2_YES:-}" = 1 ]; then a=y
  else read -rp "Proceed? [y/N]: " a 2>/dev/null </dev/tty || a=n; fi
  case "$a" in [yY]|[yY][eE][sS]) ;; *) warn "Upgrade cancelled — nothing changed."; return 0 ;; esac
  hr
  auto_backup
  install_prereqs
  install_binary force
  for u in $units; do
    use_unit "$u"
    echo >&2; info "Tunnel $u"
    if [ ! -f "$CFG" ]; then warn "$u: its config $CFG is missing — skipped (delete it in the Tunnel manager)."; continue; fi
    migrate_config
    # Old installs have an older unit: rewrite it (same config path) and make
    # sure it starts on boot.
    role=kharej; grep -q '"mode"[[:space:]]*:[[:space:]]*"dial"' "$CFG" && role=iran
    write_service "$role"
    systemctl enable "$u" >/dev/null 2>&1 || true
    if restart_unit "$u"; then
      ok "$u upgraded and running (autostart: $(systemctl is-enabled "$u" 2>/dev/null || true))."
      if ! verify_tunnel "$CFG" "${HS2_VERIFY_SECS:-60}"; then
        warn "$u has not reconnected yet. If the OTHER server still runs the old version, upgrade it"
        warn "too — it reconnects then. If both are upgraded and it stays down: journalctl -u $u -n 40 --no-pager"
      fi
    else
      err "$u failed to start after the upgrade. Last log:"
      journalctl -u "$u" -n 20 --no-pager >&2 || true
      fails=$((fails + 1))
    fi
  done
  echo >&2
  # Sweep echo-guard rules left by daemons that are no longer running (keyed on
  # a dead PID). Safe in a partial upgrade too: a tunnel still on the old build
  # has a live PID, so its rule is kept.
  kernel_cleanup
  [ "$fails" = 0 ] || { err "$fails tunnel(s) did not start — see above."; bail; }
  ok "hs2 upgraded. Upgrade the OTHER server(s) too (both sides of a tunnel must match)."
  info "Tunnel manager: run  hs2-menu  → 3"
}

# Non-interactive: bash install.sh upgrade   (or: curl … | bash -s upgrade)
case "${1:-}" in
  ''|menu)   ;;                                   # no argument → the interactive menu below
  manage)    tunnel_manager; exit 0 ;;
  upgrade)   upgrade "${2:-}"; exit 0 ;;
  backup)    backup >/dev/null; exit 0 ;;
  restore)   restore "${2:-}"; exit 0 ;;
  status)    status; exit 0 ;;
  uninstall) uninstall; exit 0 ;;
  cleanup)   kernel_cleanup; exit 0 ;;
  version)   "$BIN" version 2>/dev/null || die "hs2 is not installed on this server."; exit 0 ;;
  *) err "unknown command: $1"
     say "Usage: bash install.sh [manage | upgrade [tunnel] | backup | restore [file] | status | uninstall | cleanup | version]"
     say "       (no argument opens the menu)"
     # HS2_DIED=1 like die/bail: this is a deliberate usage error, so the EXIT
     # trap must NOT add its "stopped unexpectedly / report to the developer" note.
     HS2_DIED=1; exit 2 ;;
esac

# ---------- menu -------------------------------------------------------------
main_menu(){
  local CH bld
  while :; do
    echo >&2
    _c '1;36' "╔══════════════════════════════════════════╗"
    _c '1;36' "║   hs2 v3 — DPI-resistant tunnel           ║"
    _c '1;36' "║   runs alongside Backhaul                 ║"
    _c '1;36' "╚══════════════════════════════════════════╝"
    # Show the build of the shared binary installed here, so an operator can
    # confirm Iran and kharej run the same one. Blank for an older/unstamped
    # binary, or before any binary is installed.
    bld=$(bin_build "$BIN")
    if [ -n "$bld" ]; then _c '0;36' "   $bld"; fi
    echo >&2
    echo "  Set up a tunnel" >&2
    echo "    1) Kharej  (foreign server — panel side)" >&2
    echo "    2) Iran    (opens user ports → panel)" >&2
    echo "  Manage" >&2
    echo "    3) Tunnel manager  (list · start/stop/restart · edit · logs · delete)" >&2
    echo "    4) Status / logs" >&2
    echo "    5) Upgrade (new binary, keep config)" >&2
    echo "    6) Backup current config" >&2
    echo "    7) Restore a backup" >&2
    echo "    8) Uninstall hs2 (removes ALL tunnels)" >&2
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

#!/bin/bash
# sandbox.sh CMD [ARGS...] — run an installer test where it cannot touch the
# tunnels of the machine it runs on, and prove that before the test starts.
# (On 2026-10-26 helpers_test.sh's kill_this_tunnel case sent SIGTERM to the
# live hs2 of a production server: never again.)
#
#  * a new PID namespace: no process outside it is visible, so nothing the test
#    runs can signal a running tunnel (pgrep/kill see only the test's own), and
#    every process the test leaves behind dies with it;
#  * a new mount namespace where /etc and /usr are read-only and every path an
#    hs2 install owns is an empty temp directory: /etc/hs2, /etc/systemd/system,
#    /etc/modules-load.d, /etc/sysctl.d, /etc/letsencrypt, /usr/local/bin,
#    /usr/local/sbin, /run/hs2, /root/hs2-backups, and the hs2-safe-apply
#    result and log files; /run/systemd, /run/dbus, /run/netns and the journal
#    (/run/log/journal, /var/log/journal) are empty, so a real `systemctl`
#    cannot reach systemd, `journalctl -u hs2` shows nothing of the live
#    tunnel, and the test's network namespaces stay inside.
#
# Before the command runs, every point above is checked from inside; if one
# fails, the command does not run (exit 97). After it, from outside: no file of
# the real install changed (names, modes, sizes, times — the live status files
# the running tunnels rewrite every few seconds only by name) and no hs2 unit's
# MainPID, NRestarts or state moved; the evidence is printed, and a difference
# fails the run (exit 98). The command sees HS2_SANDBOX=proven.
#
# Without root (or without unshare) the command runs as it is, with
# HS2_SANDBOX=nonroot: it then cannot touch root's files or processes.
set -uo pipefail
PROTECT_DIRS="/etc/hs2 /etc/systemd/system /etc/modules-load.d /etc/sysctl.d /etc/letsencrypt /usr/local/bin /usr/local/sbin /run/hs2 /root/hs2-backups"
PROTECT_FILES="/root/hs2-safe-apply.last /root/hs2-safe-apply.log"
EMPTY_DIRS="/run/systemd /run/dbus /run/netns /run/log/journal /var/log/journal"
PROBE=.hs2-sandbox-probe

# realstate: what must not change — every hs2 unit's MainPID/NRestarts/state and
# every file of the real install.
realstate(){
  local f u
  for f in /etc/systemd/system/hs2.service /etc/systemd/system/hs2-*.service; do
    [ -e "$f" ] || continue
    u=$(basename "$f" .service)
    echo "unit $u $(systemctl show -p MainPID -p NRestarts -p ActiveState "$u" 2>/dev/null | sort | tr '\n' ' ')"
  done
  find $PROTECT_DIRS -maxdepth 3 \( -path /run/hs2 -prune -o -print0 \) 2>/dev/null \
    | xargs -0 -r stat -c '%n %a %s %Y' 2>/dev/null | sort
  for f in $PROTECT_FILES; do [ -e "$f" ] && stat -c '%n %a %s %Y' "$f"; done
  # /run/hs2: the live status/warm files of running tunnels change by the second:
  # their names only; anything else in full.
  find /run/hs2 -mindepth 1 -maxdepth 2 2>/dev/null | sort | while read -r f; do
    case "$f" in /run/hs2/etc-hs2-*) echo "$f" ;; *) stat -c '%n %a %s %Y' "$f" ;; esac
  done
  return 0
}

if [ "${1:-}" = __inner ]; then
  shift
  fail(){ echo "sandbox NOT proven: $* — the test does not run" >&2; exit 97; }
  i=0
  for d in /etc /usr; do
    mount --bind "$d" "$d" && mount -o remount,bind,ro "$d" || fail "cannot make $d read-only"
  done
  for p in $PROTECT_DIRS; do
    [ -d "$p" ] || continue
    i=$((i + 1)); mkdir -p "$HS2_SBX_BASE/d$i"
    mount --bind "$HS2_SBX_BASE/d$i" "$p" || fail "cannot cover $p"
  done
  for f in $PROTECT_FILES; do
    [ -f "$f" ] || continue
    i=$((i + 1)); : > "$HS2_SBX_BASE/f$i"
    mount --bind "$HS2_SBX_BASE/f$i" "$f" || fail "cannot cover $f"
  done
  for d in $EMPTY_DIRS; do
    # (only /run/netns is made when missing — `ip netns add` would make it anyway)
    [ -d "$d" ] || { [ "$d" = /run/netns ] && mkdir -p "$d"; } || continue
    mount -t tmpfs -o mode=755,size=8m hs2sbx "$d" || fail "cannot empty $d"
  done
  # --- the proof ---------------------------------------------------------------
  [ "$$" = 1 ] || fail "not the first process of a new PID namespace"
  [ -z "$(pgrep -x hs2 2>/dev/null)" ] || fail "an hs2 process outside is visible"
  for pid in $HS2_SBX_PIDS; do kill -0 "$pid" 2>/dev/null && fail "the running tunnel's process $pid is reachable"; done
  for d in /etc /usr; do
    touch "$d/$PROBE" 2>/dev/null && { rm -f "$d/$PROBE"; fail "$d is writable"; }
  done
  for p in $PROTECT_DIRS; do
    [ -d "$p" ] || continue
    id=$(stat -c %d:%i "$p")
    case " $HS2_SBX_IDS " in *" $p=$id "*) fail "$p is the real one" ;; esac
    [ -z "$(ls -A "$p")" ] || fail "$p is not empty"
    touch "$p/$PROBE" || fail "$p cannot take the test's files"
  done
  for f in $PROTECT_FILES; do [ -e "$f" ] && [ -s "$f" ] && fail "$f is the real one"; done
  [ -d /run/systemd/system ] && fail "/run/systemd is the real one"
  systemctl show -p MainPID --value hs2 2>/dev/null | grep -q '[1-9]' && fail "systemctl still reaches systemd"
  journalctl -q -u hs2 -n 1 --no-pager 2>/dev/null | grep -q . && fail "the live tunnel's journal is visible"
  echo "sandbox: proven — PID namespace (no outside process visible or reachable), /etc and /usr read-only," \
       "the install's paths are empty temp dirs, systemctl cannot reach systemd" >&2
  HS2_SANDBOX=proven "$@"
  exit $?
fi

[ $# -gt 0 ] || { echo "usage: sandbox.sh CMD [ARGS...]" >&2; exit 2; }
if [ "$(id -u)" != 0 ] || ! command -v unshare >/dev/null 2>&1; then
  echo "sandbox: not root (or no unshare) — running as is; this user cannot touch root's tunnels" >&2
  HS2_SANDBOX=nonroot exec "$@"
fi
base=$(mktemp -d /tmp/hs2-sandbox.XXXXXX); chmod 755 "$base"
ids=""; for p in $PROTECT_DIRS; do [ -d "$p" ] && ids="$ids $p=$(stat -c %d:%i "$p")"; done
pids=""
for f in /etc/systemd/system/hs2.service /etc/systemd/system/hs2-*.service; do
  [ -e "$f" ] || continue
  p=$(systemctl show -p MainPID --value "$(basename "$f" .service)" 2>/dev/null)
  case "$p" in ''|0) ;; *) pids="$pids $p" ;; esac
done
pids="$pids $(pgrep -x hs2 2>/dev/null | tr '\n' ' ')"
before=$(realstate)
HS2_SBX_BASE="$base" HS2_SBX_IDS="$ids" HS2_SBX_PIDS="$pids" \
  unshare --mount --pid --fork --mount-proc --propagation private -- bash "$0" __inner "$@"
rc=$?
after=$(realstate)
leak=""
for p in /etc /usr $PROTECT_DIRS; do [ -e "$p/$PROBE" ] && leak="$leak $p/$PROBE"; done
rm -rf "$base"
echo "sandbox evidence (real install, before → after):" >&2
printf '%s\n' "$before" | grep '^unit ' | sed 's/^/  before: /' >&2
printf '%s\n' "$after" | grep '^unit ' | sed 's/^/  after:  /' >&2
if [ -n "$leak" ]; then
  echo "  a probe reached the real paths:$leak" >&2; exit 98
fi
if [ "$before" != "$after" ]; then
  echo "  CHANGED:" >&2; diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") | sed 's/^/    /' >&2
  exit 98
fi
echo "  unchanged: $(printf '%s\n' "$before" | grep -vc '^unit ') files of the real install, and every hs2 unit's MainPID/NRestarts/state" >&2
exit $rc

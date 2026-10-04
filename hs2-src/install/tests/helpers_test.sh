#!/bin/bash
# Installer helpers that act on the system, tested for real against a sandbox
# (no systemd needed; root only for nothing — any user can run it):
#   verify_download  — the published sha256 is enforced (match / mismatch /
#                      missing file) with curl over file:// as the "repository";
#   kill_this_tunnel — stops THIS config's hs2 only, never another tunnel's
#                      (the old uninstall ran `pkill -x hs2`);
#   prune_backups    — keeps the newest N backups (each holds the tunnel key).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"; SRC=$(cd "$HERE/../.." && pwd)
T=$(mktemp -d); trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

sed -n '/^verify_download(){/,/^}/p;/^kill_this_tunnel(){/,/^}/p;/^prune_backups(){/,/^}/p' "$INST" > "$T/fns.sh"
for f in verify_download kill_this_tunnel prune_backups; do
  grep -q "^$f(){" "$T/fns.sh" || { echo "FAIL $f not found in install.sh"; exit 1; }
done
cat > "$T/run.sh" <<RUN
set -euo pipefail
ok(){ echo "OK: \$*" >&2; }; warn(){ echo "WARN: \$*" >&2; }; err(){ echo "ERR: \$*" >&2; }; info(){ echo "INFO: \$*" >&2; }
source "$T/fns.sh"
RUN

# ---- verify_download --------------------------------------------------------
mkdir -p "$T/repo"; head -c 100000 /dev/urandom > "$T/repo/hs2-linux-amd64"; cp "$T/repo/hs2-linux-amd64" "$T/dl"
# Prints the EXACT status: 0 match · 1 mismatch · 2 no usable published hash.
vd(){ bash -c "source '$T/run.sh'; REPO_RAW='file://$T/repo'; if verify_download '$T/dl'; then echo RC=0; else echo RC=\$?; fi" 2>&1; }
sha256sum "$T/repo/hs2-linux-amd64" | sed "s#  .*#  hs2-linux-amd64#" > "$T/repo/hs2-linux-amd64.sha256"
out=$(vd); check "verify_download: a download matching the published sha256 passes" 'echo "$out" | grep -q RC=0 && echo "$out" | grep -q "matches"'
printf 'x' >> "$T/dl"
out=$(vd); check "verify_download: an altered download is rejected (status 1)" 'echo "$out" | grep -qx RC=1 && echo "$out" | grep -q "mismatch"'
rm "$T/repo/hs2-linux-amd64.sha256"
# Fail-closed (since A1): no published hash is status 2, never a pass —
# install_binary then refuses unless HS2_ALLOW_UNVERIFIED=1.
out=$(vd); check "verify_download: no published hash is status 2 (not a pass)" 'echo "$out" | grep -qx RC=2'
echo "not-a-hash" > "$T/repo/hs2-linux-amd64.sha256"
out=$(vd); check "verify_download: a garbage hash file counts as missing (status 2), not as a pass-by-accident" 'echo "$out" | grep -qx RC=2'

# ---- kill_this_tunnel ---------------------------------------------------------
if command -v go >/dev/null 2>&1; then
  mkdir -p "$T/fake"
  printf 'package main\nimport "time"\nfunc main(){ time.Sleep(60*time.Second) }\n' > "$T/fake/main.go"
  (cd "$T/fake" && GOFLAGS= GO111MODULE=off go build -o "$T/hs2" main.go) >/dev/null 2>&1
fi
if [ -x "$T/hs2" ]; then
  "$T/hs2" run -c /etc/hs2/config.json & mine=$!
  "$T/hs2" run -c /etc/hs2/config.json.bak & similar=$!
  "$T/hs2" run -c /etc/hs2/other.json & other=$!
  sleep 0.3
  bash -c "source '$T/run.sh'; CFG=/etc/hs2/config.json; kill_this_tunnel TERM ''" 2>/dev/null
  sleep 0.3
  alive(){ kill -0 "$1" 2>/dev/null && [ "$(ps -o stat= -p "$1" 2>/dev/null | cut -c1)" != Z ]; }
  check "kill_this_tunnel: this config's hs2 is stopped" '! alive $mine'
  check "kill_this_tunnel: another tunnel's hs2 keeps running" 'alive $other'
  check "kill_this_tunnel: a config whose path only starts the same keeps running" 'alive $similar'
  # the unit's last PID is signalled only when it still is an hs2 process
  sleep 60 & notours=$!
  bash -c "source '$T/run.sh'; CFG=/nonexistent; kill_this_tunnel TERM $notours; kill_this_tunnel TERM $other" 2>/dev/null
  sleep 0.3
  check "kill_this_tunnel: a recycled PID that is not hs2 is left alone" 'alive $notours'
  check "kill_this_tunnel: the unit's PID is stopped when it is hs2" '! alive $other'
  kill $similar $notours 2>/dev/null
else
  echo "SKIP kill_this_tunnel (no go toolchain to build a stand-in hs2)"
fi

# ---- prune_backups ------------------------------------------------------------
mkdir -p "$T/bk"
for i in $(seq -w 1 13); do
  f="$T/bk/hs2-host-202609${i}-000000.tar.gz"; : > "$f"; touch -d "2026-09-$i 00:00" "$f"
done
: > "$T/bk/unrelated.txt"
out=$(bash -c "source '$T/run.sh'; BACKUP_DIR='$T/bk'; BACKUP_KEEP=10; prune_backups" 2>&1)
check "prune_backups: keeps exactly the newest 10" '[ "$(ls "$T/bk"/hs2-*.tar.gz | wc -l)" = 10 ] && [ ! -e "$T/bk/hs2-host-20260901-000000.tar.gz" ] && [ ! -e "$T/bk/hs2-host-20260903-000000.tar.gz" ] && [ -e "$T/bk/hs2-host-20260904-000000.tar.gz" ] && [ -e "$T/bk/hs2-host-20260913-000000.tar.gz" ]'
check "prune_backups: says what it removed" 'echo "$out" | grep -q "Removed 3 old backup"'
check "prune_backups: other files in the folder are not touched" '[ -e "$T/bk/unrelated.txt" ]'
out=$(bash -c "source '$T/run.sh'; BACKUP_DIR='$T/bk'; BACKUP_KEEP=0; prune_backups; echo RC=\$?" 2>&1)
check "prune_backups: HS2_KEEP_BACKUPS=0 disables pruning" 'echo "$out" | grep -q RC=0 && [ "$(ls "$T/bk"/hs2-*.tar.gz | wc -l)" = 10 ]'
out=$(bash -c "source '$T/run.sh'; BACKUP_DIR='$T/empty'; BACKUP_KEEP=10; prune_backups; echo RC=\$?" 2>&1)
check "prune_backups: no backup folder is fine under set -e" 'echo "$out" | grep -q RC=0'
# ---- remove_tunnel / uninstall: no leftovers in /run/hs2 ----------------------
# (a real-server report: /run/hs2/etc-hs2-config.json.warm stayed after a full
# uninstall). The warm-start record is named like cmd/hs2's warmPath: the status
# file's name with .warm for .status.json.
sed -n '/^status_path(){/,/^}/p' "$INST" > "$T/sp.sh"
warm=$(bash -c "source '$T/sp.sh'; sp=\$(status_path /etc/hs2/config.json); echo \"\${sp%.status.json}.warm\"")
check "the warm record of /etc/hs2/config.json is /run/hs2/etc-hs2-config.json.warm" '[ "$warm" = /run/hs2/etc-hs2-config.json.warm ]'
check "remove_tunnel deletes the tunnel's warm record with its status file" 'sed -n "/^remove_tunnel(){/,/^}/p" "$INST" | grep -q "\"\${sp%.status.json}.warm\""'
check "uninstall clears /run/hs2 (warm records, status files, the ping-guard folder)" 'sed -n "/^uninstall(){/,/^}/p" "$INST" | grep -q "rm -f /run/hs2/\*.warm" && sed -n "/^uninstall(){/,/^}/p" "$INST" | grep -q "rmdir /run/hs2/icmp-echo-ignore"'
check "remove_tunnel deletes the unit's systemd drop-in folder (a real-server report: an empty hs2.service.d stayed)" 'sed -n "/^remove_tunnel(){/,/^}/p" "$INST" | grep -q "rm -rf \"\${UNIT_DIR:?}/\$u.service.d\""'
check "uninstall asks before removing the program and hs2-menu (default: keep)" 'sed -n "/^uninstall(){/,/^}/p" "$INST" | grep -q "rm -f \"\$BIN\" \"\$MENU_BIN\""'

exit $fail

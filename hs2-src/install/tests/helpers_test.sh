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
# ---- cfg_put: config replace is atomic (rev53a) -------------------------------
# The menus used `cat NEW > CFG`, which truncates CFG first: on a full disk the
# tunnel's config was left empty/half-written, and the rollback `cat CFG.prev >
# CFG` failed the same way. cfg_put writes beside CFG and renames only a
# complete copy; on failure CFG is untouched and the status is non-zero.
sed -n '/^cfg_put(){/,/^}/p' "$INST" > "$T/cp.sh"
check "cfg_put exists in install.sh" 'grep -q "^cfg_put(){" "$T/cp.sh"'
pat='cat "[$](cfg|tmp|new|1)([.]prev)?" > "[$](cfg|1)"'
check "no menu path writes a tunnel config with a truncating cat any more" '! grep -nE "$pat" "$INST"'
mkdir -p "$T/cp"; printf '{"a":1}\n' > "$T/cp/c.json"; chmod 600 "$T/cp/c.json"; printf '{"a":2}\n' > "$T/cp/n.json"
bash -c "source '$T/cp.sh'; cfg_put '$T/cp/n.json' '$T/cp/c.json'"; rc=$?
check "cfg_put: replaces the config, keeps mode 600, leaves no temp file" '[ $rc = 0 ] && grep -q "\"a\":2" "$T/cp/c.json" && [ "$(stat -c %a "$T/cp/c.json")" = 600 ] && [ "$(ls "$T/cp" | wc -l)" = 2 ]'
ln -s c.json "$T/cp/link.json"; printf '{"a":3}\n' > "$T/cp/n.json"
bash -c "source '$T/cp.sh'; cfg_put '$T/cp/n.json' '$T/cp/link.json'"
check "cfg_put: a symlinked config is written through (the link stays a link)" '[ -L "$T/cp/link.json" ] && grep -q "\"a\":3" "$T/cp/c.json"'
bash -c "source '$T/cp.sh'; cfg_put '$T/cp/missing.json' '$T/cp/c.json'"; rc=$?
check "cfg_put: a failed copy returns non-zero and leaves the config intact" '[ $rc != 0 ] && grep -q "\"a\":3" "$T/cp/c.json" && [ "$(ls "$T/cp" | wc -l)" = 3 ]'
if [ "$(id -u)" = 0 ] && command -v unshare >/dev/null 2>&1; then
  # A REAL full disk: a 64 KiB tmpfs in a private mount namespace.
  # (deterministic filler: random data could contain "keep" by chance)
  head -c 200000 /dev/zero | tr '\0' x > "$T/cp/big.json"
  mkdir -p "$T/cp/fs"
  out=$(unshare -m bash -c "mount -t tmpfs -o size=64k tmpfs '$T/cp/fs' || exit 9
    printf '{\"keep\":1}\n' > '$T/cp/fs/c.json'
    source '$T/cp.sh'
    if cfg_put '$T/cp/big.json' '$T/cp/fs/c.json'; then echo PUT=0; else echo PUT=1; fi
    cat '$T/cp/fs/c.json'; ls '$T/cp/fs' | wc -l" 2>&1)
  check "cfg_put on a FULL disk: fails, the config keeps its old content, no temp left" 'echo "$out" | grep -qx PUT=1 && echo "$out" | grep -qx "{\"keep\":1}" && echo "$out" | tail -1 | grep -qx 1'
  # the old way, for contrast: the truncating cat destroys the config
  out=$(unshare -m bash -c "mount -t tmpfs -o size=64k tmpfs '$T/cp/fs' || exit 9
    printf '{\"keep\":1}\n' > '$T/cp/fs/c.json'; cat '$T/cp/big.json' > '$T/cp/fs/c.json' 2>/dev/null; grep -c keep '$T/cp/fs/c.json'" 2>&1)
  check "(contrast) the old truncating cat on a full disk loses the config" 'echo "$out" | tail -1 | grep -qx 0'
else
  echo "SKIP cfg_put full-disk test (needs root + unshare)"
fi
# ---- drop-in ExecStart overrides (rev53a) -------------------------------------
# A classic install runs a tunnel through a drop-in such as 40-tunnel1.conf
# (ExecStart= reset + ExecStart=/opt/x/hs2 run -c /etc/hs2/t1.json). tm_cfg read
# only the unit file (wrong config), and upgrade replaced $BIN but restarted the
# tunnel on the drop-in's OLD binary while reporting it "upgraded".
sed -n '/^tm_cfg(){/,/^}/p;/^tm_dropin_execs(){/,/^}/p;/^dropin_exec_sync(){/,/^}/p' "$INST" > "$T/di.sh"
for f in tm_cfg tm_dropin_execs dropin_exec_sync; do check "$f exists in install.sh" "grep -q '^$f(){' '$T/di.sh'"; done
SD="$T/sd"; mkdir -p "$SD/hs2-t1.service.d" "$SD/hs2-plain.service.d"
printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/hs2-t1.json\n' > "$SD/hs2-t1.service"
printf '[Service]\nExecStart=\nExecStart=/opt/hs2-old/hs2 run -c /etc/hs2/tunnel1.json\n' > "$SD/hs2-t1.service.d/40-tunnel1.conf"
printf '[Service]\nCPUAffinity=0 1\n' > "$SD/hs2-t1.service.d/50-cpu.conf"
printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/plain.json\n' > "$SD/hs2-plain.service"
printf '[Service]\nLimitNOFILE=4096\n' > "$SD/hs2-plain.service.d/10-limits.conf"
cat > "$T/di_run.sh" <<RUN
set -euo pipefail
warn(){ echo "WARN: \$*" >&2; }; systemctl(){ echo "systemctl \$*" >> "$T/sdlog"; }
UNIT_DIR="$SD"; BIN=/usr/local/bin/hs2
source "$T/di.sh"
RUN
out=$(bash -c "source '$T/di_run.sh'; tm_cfg hs2-t1; tm_cfg hs2-plain; tm_cfg hs2-none; echo END" 2>&1)
check "tm_cfg: a drop-in's ExecStart -c wins over the unit file" '[ "$(echo "$out" | sed -n 1p)" = /etc/hs2/tunnel1.json ]'
check "tm_cfg: a unit without an ExecStart drop-in keeps its own -c" '[ "$(echo "$out" | sed -n 2p)" = /etc/hs2/plain.json ]'
check "tm_cfg: an unknown unit prints nothing and is no error under set -e" '[ "$(echo "$out" | sed -n 3p)" = END ]'
out=$(bash -c "source '$T/di_run.sh'; tm_dropin_execs hs2-t1; tm_dropin_execs hs2-plain; echo END" 2>&1)
check "tm_dropin_execs: only the drop-in that sets ExecStart is listed" '[ "$out" = "$(printf "%s\nEND" "$SD/hs2-t1.service.d/40-tunnel1.conf")" ]'
: > "$T/sdlog"
out=$(bash -c "source '$T/di_run.sh'; dropin_exec_sync hs2-t1; dropin_exec_sync hs2-plain; echo END" 2>&1)
check "dropin_exec_sync: the override now runs the upgraded \$BIN with its own -c" 'grep -qx "ExecStart=/usr/local/bin/hs2 run -c /etc/hs2/tunnel1.json" "$SD/hs2-t1.service.d/40-tunnel1.conf"'
check "dropin_exec_sync: the ExecStart= reset line is kept" 'grep -qx "ExecStart=" "$SD/hs2-t1.service.d/40-tunnel1.conf"'
check "dropin_exec_sync: the original is kept as .pre-upgrade (ignored by systemd)" 'grep -q /opt/hs2-old/hs2 "$SD/hs2-t1.service.d/40-tunnel1.conf.pre-upgrade"'
check "dropin_exec_sync: says what it changed and reloads systemd once" 'echo "$out" | grep -q "ran another binary" && [ "$(grep -c daemon-reload "$T/sdlog")" = 1 ]'
check "dropin_exec_sync: other drop-ins are untouched, no temp files left" '[ "$(cat "$SD/hs2-t1.service.d/50-cpu.conf")" = "$(printf "[Service]\nCPUAffinity=0 1")" ] && [ "$(ls "$SD/hs2-t1.service.d" | wc -l)" = 3 ] && [ "$(ls "$SD/hs2-plain.service.d" | wc -l)" = 1 ]'
: > "$T/sdlog"
out=$(bash -c "source '$T/di_run.sh'; dropin_exec_sync hs2-t1; echo END" 2>&1)
check "dropin_exec_sync: a second run changes nothing (idempotent, no reload)" '! echo "$out" | grep -q "ran another binary" && [ ! -s "$T/sdlog" ]'
check "upgrade points drop-ins at the new binary and checks the running exe" 'sed -n "/^upgrade(){/,/^}/p" "$INST" | grep -q "dropin_exec_sync \"\$u\"" && sed -n "/^upgrade(){/,/^}/p" "$INST" | grep -q "running_exe_ok \"\$u\""'
check "backup includes the unit's drop-in folder" 'sed -n "/^backup(){/,/^}/p" "$INST" | grep -qF "\$u.service.d\")"'

# ---- helper units are not tunnels; their names are reserved (rev53a) ----------
# tm_units globbed hs2-*.service, so hs2-stats / hs2-lb-park / hs2-alert-<u>
# showed up as (broken) tunnels and upgrade/backup/uninstall walked them; and a
# tunnel named "stats" got its unit overwritten when the sampler was enabled.
sed -n '/^tm_units(){/,/^}/p;/^norm_unit(){/,/^}/p' "$INST" > "$T/un.sh"
U2="$T/units"; mkdir -p "$U2"
printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/config.json\n' > "$U2/hs2.service"
printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/hs2-de1.json\n' > "$U2/hs2-de1.service"
printf '[Service]\nType=oneshot\nExecStart=/usr/local/bin/hs2 stats sample\n' > "$U2/hs2-stats.service"
printf '[Service]\nType=oneshot\nExecStart=/usr/local/bin/hs2 lb park --sweep\n' > "$U2/hs2-lb-park.service"
printf '[Service]\nType=oneshot\nExecStart=/usr/local/bin/hs2 alert -c /etc/hs2/hs2-de1.json -u hs2-de1\n' > "$U2/hs2-alert-hs2-de1.service"
printf '[Service]\nExecStart=/usr/local/bin/hs2-coreguard --watch\n' > "$U2/hs2-coreguard.service"
ln -s /dev/null "$U2/hs2-masked.service"
out=$(bash -c "set -euo pipefail; UNIT_DIR='$U2'; source '$T/un.sh'; tm_units | tr '\n' ' '" 2>&1)
check "tm_units: only tunnels (and a masked tunnel) are listed, not hs2's helper units" '[ "$out" = "hs2 hs2-de1 hs2-masked " ]'
printf '[Service]\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/hs2-stats.json\n' > "$U2/hs2-stats.service"
out=$(bash -c "set -euo pipefail; UNIT_DIR='$U2'; source '$T/un.sh'; tm_units | tr '\n' ' '" 2>&1)
check "tm_units: an older TUNNEL that happens to be named hs2-stats is still listed" 'echo "$out" | grep -q "hs2-stats "'
for n in stats lb-park coreguard alert-hs2 hs2-alert-x menu; do
  r=$(bash -c "source '$T/un.sh'; norm_unit '$n' && echo ACCEPTED || echo REJECTED" 2>&1)
  check "norm_unit: the helper name '$n' is rejected as a tunnel name" '[ "$r" = REJECTED ]'
done
for n in de1 stats2 alerts lb; do
  r=$(bash -c "source '$T/un.sh'; norm_unit '$n'" 2>&1)
  check "norm_unit: '$n' is still a valid tunnel name" '[ "$r" = "hs2-$n" ]'
done
check "remove_tunnel also removes the tunnel's alert timer" 'sed -n "/^remove_tunnel(){/,/^}/p" "$INST" | grep -q "disable --now \"hs2-alert-\$u.timer\""'
check "uninstall also removes the helper timers (hs2-stats, hs2-lb-park)" 'sed -n "/^uninstall(){/,/^}/p" "$INST" | grep -q "for h in hs2-stats hs2-lb-park"'

# ---- parse_link: a tampered link cannot inject JSON (rev53a) -------------------
# Link fields go verbatim into JSON strings (and udp as a bare value) of this
# server's config; a '"' let a link add keys that still parse (last key wins).
sed -n '/^decode_link(){/p;/^valid_uint(){/,/^}/p;/^parse_link(){/,/^}/p' "$INST" | sed 's#</dev/tty#<"$LINKIN"#' > "$T/pl.sh"
mklink(){ printf '%s' "$1" | base64 -w0; }
pl(){ # decoded link -> PARSED:<sni>|<udp> or the die message
  printf 'hs2://%s\n' "$(mklink "$1")" > "$T/linkin"
  bash -c "set -euo pipefail; die(){ echo \"DIE: \$*\"; exit 1; }; LINKIN='$T/linkin'; source '$T/pl.sh'; parse_link; echo \"PARSED:\$DOMAIN|\$UDP|\$SHARED\"" 2>&1
}
out=$(pl '203.0.113.5:443|t.example.com|c2VjcmV0K2tleS89|127.0.0.1:8443|mtcp|false|tcp|direct|1380||||')
check "parse_link: a normal link parses" '[ "$out" = "PARSED:t.example.com|false|c2VjcmV0K2tleS89" ]'
out=$(pl '[2001:db8::5]:443|t.example.com|abc|127.0.0.1:8443|mtcp|true|tcp|direct|||||')
check "parse_link: an IPv6 endpoint link parses" 'echo "$out" | grep -q "^PARSED:t.example.com|true|abc$"'
out=$(pl '203.0.113.5:443|x","forward_ports":"22|abc|127.0.0.1:8443|mtcp|false|tcp|direct|||||')
check "parse_link: a '\"' in a field (JSON key injection) is refused" 'echo "$out" | grep -q "^DIE: the link carries an invalid character"'
out=$(pl '203.0.113.5:443|t.example.com|ab\c|127.0.0.1:8443|mtcp|false|tcp|direct|||||')
check "parse_link: a backslash in a field is refused" 'echo "$out" | grep -q "^DIE:"'
out=$(pl '203.0.113.5:443|t.example.com|abc|127.0.0.1:8443|mtcp|true,"forward_ports":"22"|tcp|direct|||||')
check "parse_link: udp must be exactly true/false (it is a bare JSON value)" 'echo "$out" | grep -q "^DIE:"'
out=$(pl '203.0.113.5:443|t.example.com|abc|127.0.0.1:8443|mtcp|true,x:1|tcp|direct|||||')
check "parse_link: udp with no quote at all is refused too" 'echo "$out" | grep -q "^DIE: the link carries an invalid udp field"'


exit $fail

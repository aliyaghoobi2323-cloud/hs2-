#!/bin/bash
# f379: b378 made `hs2 tunnel renew --upgrade-from SRC` trust SRC.sha256 only
# on a local disk fs (ext4/xfs/btrfs, root-owned overlay) through root-owned,
# non-writable dirs; on tmpfs (/tmp, /dev/shm), ZFS and the rest it needs
# --sha256. The installer's own upgrade/setup paths must not depend on that:
#   - no install.sh path calls `--upgrade-from` without `--sha256`;
#   - install_binary (menu Upgrade, `install.sh upgrade`, setup) verifies the
#     download itself and installs it with mktemp staging in a world-writable
#     sticky tmpfs TMPDIR (a normal Ubuntu /tmp), online and offline;
#   - a wrong hash is still refused there and $BIN stays as it was.
# Runs as any user; as root the staging dir is a real fresh tmpfs mount.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); MNT=""
trap '[ -n "$MNT" ] && umount "$MNT" 2>/dev/null; rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

# 1. every --upgrade-from in the installer passes the hash it verified
bad=$(grep -n -- '--upgrade-from' "$INST" | grep -v '^[0-9]*:[[:space:]]*#' | grep -v -- '--sha256' || true)
check "install.sh never calls --upgrade-from without --sha256" '[ -z "$bad" ]'
check "the menu's renew (8) does not upgrade the binary through renew" \
  'grep -q "nm_tun renew -u \"\$u\" --pick-ip;" "$INST" && ! grep -n "nm_tun renew" "$INST" | grep -q -- "--upgrade-from"'
up=$(sed -n '/^upgrade(){/,/^}/p' "$INST")
check "upgrade() installs through install_binary force" 'echo "$up" | grep -q "^  install_binary force$"'

# 2. a tmpfs staging dir like /tmp: world-writable + sticky
S="$T/tmpfs"; mkdir -p "$S"
if [ "$(id -u)" = 0 ] && mount -t tmpfs -o mode=1777 tmpfs "$S" 2>/dev/null; then MNT="$S"
elif [ "$(stat -f -c %T /dev/shm 2>/dev/null)" = tmpfs ] && [ -w /dev/shm ]; then
  rmdir "$S"; S=$(mktemp -d /dev/shm/hs2stage.XXXXXX); chmod 1777 "$S"
  trap '[ -n "$MNT" ] && umount "$MNT" 2>/dev/null; rm -rf "$T" "$S"' EXIT
else chmod 1777 "$S"; echo "NOTE no tmpfs available: staging in a sticky 1777 dir on $(stat -f -c %T "$S")"; fi
echo "staging dir $S: fs $(stat -f -c %T "$S"), mode $(stat -c %a "$S")"

sed -n '/^verify_local(){/,/^}/p;/^hs2_is_v3(){/p;/^verify_download(){/,/^}/p;/^install_binary(){/,/^}/p' "$INST" > "$T/fns.sh"
for f in verify_local hs2_is_v3 verify_download install_binary; do
  grep -q "^$f(){" "$T/fns.sh" || { echo "FAIL $f not found in install.sh"; exit 1; }
done
mkdir -p "$T/bin" "$T/inst" "$T/rel"
mkbin(){ printf '#!/bin/sh\n[ "$1" = version ] && echo "hs2 v3.%s (test)"\n' "$2" > "$1"; chmod 755 "$1"; }
mkbin "$T/rel/hs2-linux-amd64" 99
sha256sum "$T/rel/hs2-linux-amd64" | sed 's#  .*#  hs2-linux-amd64#' > "$T/rel/hs2-linux-amd64.sha256"
# curl stand-in: serves $T/rel; CURL_MODE=offline fails every fetch,
# CURL_MODE=badhash serves a wrong hash; every -o target is logged.
cat > "$T/bin/curl" <<EOF
#!/bin/bash
o=""; url=""
while [ \$# -gt 0 ]; do case "\$1" in -o) o="\$2"; shift 2 ;; --connect-timeout|--retry) shift 2 ;; -*) shift ;; *) url="\$1"; shift ;; esac; done
[ "\${CURL_MODE:-}" = offline ] && exit 7
[ -n "\$o" ] && echo "\$o" >> "$T/curl-o"
f="$T/rel/\$(basename "\${url%%\\?*}")"
[ -f "\$f" ] || exit 22
if [ "\${CURL_MODE:-}" = badhash ] && [ "\${f%.sha256}" != "\$f" ]; then printf '%064d  hs2-linux-amd64\n' 0; exit 0; fi
if [ -n "\$o" ]; then cp "\$f" "\$o"; else cat "\$f"; fi
EOF
chmod 755 "$T/bin/curl"
run(){ # mode [cwd]
  ( cd "${2:-$T}" && CURL_MODE="$1" TMPDIR="$S" PATH="$T/bin:$PATH" bash -c "set -euo pipefail
info(){ echo INFO: \$* >&2; }; ok(){ echo OK: \$* >&2; }; warn(){ echo WARN: \$* >&2; }; err(){ echo ERR: \$* >&2; }
say(){ echo \"\$*\" >&2; }; hr(){ :; }; die(){ echo DIE: \$* >&2; exit 1; }; install_self(){ echo SELF >&2; }
BIN='$T/inst/hs2'; REPO_RAW=https://example.invalid/raw
source '$T/fns.sh'; install_binary force" 2>&1 ); }

mkbin "$T/inst/hs2" 1
: > "$T/curl-o"
out=$(run online); rc=$?
check "online upgrade with TMPDIR on tmpfs: installed and verified" \
  '[ $rc = 0 ] && "$T/inst/hs2" version | grep -q "v3.99" && echo "$out" | grep -q "sha256 matches the published hash"'
check "the download really was staged in the tmpfs dir (mktemp in TMPDIR)" 'grep -q "^$S/" "$T/curl-o"'
check "nothing left behind in the staging dir or beside \$BIN" \
  '[ -z "$(ls -A "$S")" ] && [ -z "$(ls "$T/inst" | grep -v "^hs2$")" ]'

mkbin "$T/inst/hs2" 1
out=$(run badhash); rc=$?
check "a wrong published hash is refused (fail closed), \$BIN unchanged" \
  '[ $rc != 0 ] && echo "$out" | grep -q "NOT installed" && "$T/inst/hs2" version | grep -q "v3.1 " && [ -z "$(ls -A "$S")" ]'

# offline (Iran side): the binary + .sha256 copied by hand into a tmpfs dir
W="$S/work"; mkdir -p "$W"; cp "$T/rel/hs2-linux-amd64" "$T/rel/hs2-linux-amd64.sha256" "$W/"
out=$(run offline "$W"); rc=$?
check "offline upgrade from a copy in a tmpfs dir with its .sha256: installed" \
  '[ $rc = 0 ] && "$T/inst/hs2" version | grep -q "v3.99" && echo "$out" | grep -q "sha256 matches .*hs2-linux-amd64.sha256"'
mkbin "$T/inst/hs2" 1
printf '%064d  hs2-linux-amd64\n' 0 > "$W/hs2-linux-amd64.sha256"
out=$(run offline "$W"); rc=$?
check "offline: a copy that does not match its .sha256 is refused, \$BIN unchanged" \
  '[ $rc != 0 ] && echo "$out" | grep -q "does not match" && "$T/inst/hs2" version | grep -q "v3.1 "'
rm -rf "$W"
[ $fail = 0 ] && echo "OK upgrade_staging_test" || echo "FAIL upgrade_staging_test"
exit $fail

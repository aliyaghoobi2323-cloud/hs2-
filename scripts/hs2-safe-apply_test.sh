#!/bin/bash
# scripts/hs2-safe-apply (REDESIGN.fa.md §5.1 «بازگشت‌پذیری»), driven with
# stand-ins first in PATH: systemctl (records every call, plays the unit),
# ping (the peer answers N seconds after the restart, or never), and sleep/date
# on a virtual clock, so 60 s and 300 s waits take no real time. Every path the
# script owns (binary, unit directory, backups, log, result) is in a temp
# directory and the unit is a made-up name: no real unit or file is touched.
#
#   ok          hs2 comes up, the peer answers            → OK, the change stays
#   not-up      hs2 is not running 60 s later (or restarted meanwhile)
#                                                          → binary, config and drop-ins back
#   no-peer     the peer never answers in HS2_SAFE_PEER_WAIT (300 s) → back
#   peer-late   the peer answers after 200 s, HS2_SAFE_PEER_WAIT=400 → stays
#               (and after 350 s: back with the default 300, stays with 400)
#   bad-config  `hs2 check` refuses the staged config     → nothing applied
#   dropins     <unit>.service.d/ after a revert is exactly what it was (or absent)
#   the config is the one the unit's ExecStart runs; the ping goes to its peer_ip;
#   a tunnel without a tun (mtcp) is refused unless HS2_SAFE_PEER is given;
#   install.sh carries this very script and installs it.
#
#   bash scripts/hs2-safe-apply_test.sh      (exit 0 = all PASS)
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); ROOT=$(cd "$HERE/.." && pwd)
SA="$HERE/hs2-safe-apply"; INST="$ROOT/install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; out=""
check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-700)"; fail=1; fi; }
[ -f "$SA" ] || { echo "FAIL $SA does not exist"; exit 1; }
# Never run a script that would act on the real system: every path it writes
# must come from HS2_SAFE_* (set below into the temp directory) ...
for v in HS2_SAFE_BIN HS2_SAFE_UNIT_DIR HS2_SAFE_LOG HS2_SAFE_LAST HS2_SAFE_BACKUPS; do
  grep -q "\${$v:-" "$SA" || { echo "FAIL $SA does not take its paths from $v — not run (it would touch the real system)"; exit 1; }
done
# ... and, as root, it runs in a private mount namespace where /etc, /usr, /root
# and /var are read-only, so even a mistake cannot write there.
JAILER=""
if [ "$(id -u)" = 0 ] && command -v unshare >/dev/null 2>&1; then
  cat > "$T/jail" <<'EOF'
#!/bin/bash
# run "$@" in a private mount namespace where /etc, /usr, /root and /var are read-only
exec unshare -m --propagation private -- bash -c 'for d in /etc /usr /root /var; do mount --bind "$d" "$d" && mount -o remount,bind,ro "$d" || exit 99; done; exec "$@"' jail "$@"
EOF
  chmod 755 "$T/jail"; JAILER="$T/jail"
  if "$JAILER" touch /etc/.hs2-safe-apply-test 2>/dev/null || [ -e /etc/.hs2-safe-apply-test ]; then
    rm -f /etc/.hs2-safe-apply-test; echo "FAIL the read-only jail does not hold — not run"; exit 1
  fi
  "$JAILER" true || { echo "FAIL the read-only jail cannot be set up — not run"; exit 1; }
fi
REALDATE=$(command -v date)

mkdir -p "$T/fake"
cat > "$T/fake/date" <<EOF
#!/bin/bash
if [ "\$*" = "+%s" ]; then cat "\$CLOCK"; else exec $REALDATE "\$@"; fi
EOF
cat > "$T/fake/sleep" <<'EOF'
#!/bin/bash
s=${1%%.*}; echo $(( $(cat "$CLOCK") + ${s:-0} )) > "$CLOCK"
EOF
cat > "$T/fake/ping" <<'EOF'
#!/bin/bash
# ping -c 1 -W 2 PEER: answers once FAKE_PEER seconds have passed since the last
# restart; a missing answer costs the 2 s of -W 2.
peer=${!#}; echo "$peer" >> "$SD/pinged"
now=$(cat "$CLOCK"); t0=$(cat "$SD/restart_at" 2>/dev/null || echo 999999999)
if [ "${FAKE_PEER:-never}" != never ] && [ $(( now - t0 )) -ge "$FAKE_PEER" ]; then exit 0; fi
echo $(( now + 2 )) > "$CLOCK"; exit 1
EOF
cat > "$T/fake/systemctl" <<'EOF'
#!/bin/bash
echo "$*" >> "$SD/calls"
case "${1:-}" in
  show)
    case "${3:-}" in
      ExecStart) echo "{ path=$FAKE_BIN ; argv[]=$FAKE_BIN run -c $FAKE_CFG ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }" ;;
      NRestarts) cat "$SD/nrestarts" 2>/dev/null || echo 0 ;;
      *) echo "" ;;
    esac ;;
  is-active) cat "$SD/state" 2>/dev/null || echo active ;;
  restart)
    n=$(( $(cat "$SD/restarts" 2>/dev/null || echo 0) + 1 )); echo $n > "$SD/restarts"
    cat "$CLOCK" > "$SD/restart_at"
    echo active > "$SD/state"
    if [ "$n" = 1 ]; then
      case "${FAKE_UP:-yes}" in
        no) echo failed > "$SD/state" ;;
        flap) echo 3 > "$SD/nrestarts" ;;
      esac
    fi ;;
esac
exit 0
EOF
chmod 755 "$T/fake/"*

# a stand-in hs2: `version`, and `check` that refuses a config holding BAD
mkbin(){ printf '#!/bin/bash\n# %s\ncase "$1" in version) echo "hs2 v3 (stand-in %s)" ;; check) grep -q BAD "$3" && { echo "ERROR: bad (%s)"; exit 1; }; echo "config OK (%s)" ;; esac\n' "$1" "$1" "$1" "$1" > "$2"; chmod 755 "$2"; }
cfgjson(){ printf '{\n  "mode": "listen", "carrier": "%s", "reverse": true, "addr": "192.0.2.1:2096",\n  "local_cidr": "10.77.9.2/30", "peer_ip": "%s", "shared_key": "%s"%s\n}\n' "$1" "$2" "$(printf 'ab%.0s' $(seq 32))" "${3:-}"; }

# world NAME: a fresh unit "hs2-safetest" with a binary, a config at a
# non-default path (only its ExecStart says where), and drop-ins.
world(){
  W="$T/$1"; rm -rf "$W"; mkdir -p "$W/bin" "$W/units/hs2-safetest.service.d" "$W/etc" "$W/stage" "$W/sd" "$W/bk"
  mkbin OLDBIN "$W/bin/hs2"
  cfgjson l3mtcp 10.77.9.1 > "$W/etc/tunnel-a.json"; chmod 600 "$W/etc/tunnel-a.json"
  printf '[Service]\nExecStart=%s run -c %s\n' "$W/bin/hs2" "$W/etc/tunnel-a.json" > "$W/units/hs2-safetest.service"
  printf '[Service]\nEnvironment=HS2_TUNE_SCHED=0\n' > "$W/units/hs2-safetest.service.d/zz-test.conf"
  printf '[Service]\nLimitNOFILE=4096\n' > "$W/units/hs2-safetest.service.d/10-limits.conf"
  chmod 640 "$W/units/hs2-safetest.service.d/10-limits.conf"
  touch -d '2026-01-02 03:04:05' "$W/units/hs2-safetest.service.d/10-limits.conf"
  echo 1000000 > "$W/clock"
  mkbin NEWBIN "$W/stage/hs2"
  cfgjson l3mtcp 10.77.9.1 ', "max_links": 64' > "$W/stage/config.json"
  printf '[Service]\nEnvironment=HS2_TUNE_SCHED=1\n' > "$W/stage/dropin.conf"
}
state(){ # what a revert must bring back: binary, config, drop-in directory (names, modes, sizes, times, contents)
  { md5sum "$W/bin/hs2" "$W/etc/tunnel-a.json" | cut -d' ' -f1; stat -c '%a' "$W/etc/tunnel-a.json"
    if [ -d "$W/units/hs2-safetest.service.d" ]; then
      (cd "$W/units/hs2-safetest.service.d" && find . -printf '%P %m %s %T@\n' | sort && find . -type f -exec md5sum {} + | sort)
    else echo "no drop-in directory"; fi; } 2>&1
}
run(){ # [VAR=value ...]: the script on the world's stage, with the stand-ins
  env PATH="$T/fake:$PATH" CLOCK="$W/clock" SD="$W/sd" FAKE_BIN="$W/bin/hs2" FAKE_CFG="$W/etc/tunnel-a.json" \
    HS2_SAFE_UNIT=hs2-safetest HS2_SAFE_BIN="$W/bin/hs2" HS2_SAFE_UNIT_DIR="$W/units" \
    HS2_SAFE_LOG="$W/log" HS2_SAFE_LAST="$W/last" HS2_SAFE_BACKUPS="$W/bk" "$@" ${JAILER:+"$JAILER"} bash "$SA" "$W/stage" 2>&1
}
last(){ cat "$W/last" 2>/dev/null; }
restarts(){ cat "$W/sd/restarts" 2>/dev/null || echo 0; }
onlyours(){ ! grep -v -e 'hs2-safetest' -e '^daemon-reload$' "$W/sd/calls" 2>/dev/null | grep -q .; }

# ---- ok ------------------------------------------------------------------------
world ok; before=$(state)
out=$(run FAKE_PEER=30)
check "ok: the result is OK" 'last | grep -q "^OK "'
check "ok: the new binary, config and drop-in are in place" \
  'grep -q NEWBIN "$W/bin/hs2" && grep -q max_links "$W/etc/tunnel-a.json" && grep -q SCHED=1 "$W/units/hs2-safetest.service.d/zz-test.conf" && [ "$(restarts)" = 1 ]'
check "ok: the config is the one ExecStart runs, still 0600" '[ "$(stat -c %a "$W/etc/tunnel-a.json")" = 600 ]'
check "ok: the ping goes to the config's peer_ip" '[ "$(sort -u "$W/sd/pinged")" = 10.77.9.1 ]'
check "ok: a backup of all three was made" 'b=$(last | cut -d" " -f2); [ -x "$b/hs2" ] && [ -f "$b/config.json" ] && [ -f "$b/dropins/10-limits.conf" ]'
check "ok: only the named unit is ever touched" 'onlyours'

# ---- not-up ----------------------------------------------------------------------
world notup; before=$(state)
out=$(run FAKE_PEER=0 FAKE_UP=no)
check "not-up: REVERTED, hs2 not up" 'last | grep -q "^REVERTED hs2-not-up"'
check "not-up: binary, config and drop-ins are exactly what they were" '[ "$(state)" = "$before" ]'
check "not-up: restarted again on the old state" '[ "$(restarts)" = 2 ] && grep -q "reset-failed hs2-safetest" "$W/sd/calls"'
world flap; before=$(state)
out=$(run FAKE_PEER=0 FAKE_UP=flap)
check "not-up (restarted meanwhile): REVERTED, everything back" 'last | grep -q "^REVERTED hs2-not-up" && [ "$(state)" = "$before" ]'

# ---- no-peer -----------------------------------------------------------------------
world nopeer; before=$(state)
out=$(run FAKE_PEER=never)
check "no-peer: REVERTED after 300 s without an answer" 'last | grep -q "^REVERTED tunnel-not-back-in-300s"'
check "no-peer: everything back" '[ "$(state)" = "$before" ] && [ "$(restarts)" = 2 ]'
# the clock started at 1000000 and the change was restarted then; the revert's
# restart is the last one the stand-in recorded
check "no-peer: it waited the whole 300 s before reverting" '[ $(( $(cat "$W/sd/restart_at") - 1000000 )) -ge 300 ]'

# ---- peer-late ---------------------------------------------------------------------
world late; before=$(state)
out=$(run FAKE_PEER=200 HS2_SAFE_PEER_WAIT=400)
check "peer-late: the peer answers after 200 s, HS2_SAFE_PEER_WAIT=400 → OK, the change stays" \
  'last | grep -q "^OK " && grep -q NEWBIN "$W/bin/hs2" && [ "$(restarts)" = 1 ]'
world late350; before=$(state)
out=$(run FAKE_PEER=350)
check "peer-late: after 350 s with the default 300 → REVERTED" 'last | grep -q "^REVERTED tunnel-not-back-in-300s" && [ "$(state)" = "$before" ]'
world late350b
out=$(run FAKE_PEER=350 HS2_SAFE_PEER_WAIT=400)
check "peer-late: after 350 s with HS2_SAFE_PEER_WAIT=400 → OK" 'last | grep -q "^OK "'

# ---- bad-config ----------------------------------------------------------------------
world bad; before=$(state)
cfgjson l3mtcp 10.77.9.1 ', "BAD": 1' > "$W/stage/config.json"
out=$(run FAKE_PEER=0)
check "bad-config: REFUSED, nothing applied, nothing restarted" \
  'last | grep -q "^REFUSED config-check" && [ "$(state)" = "$before" ] && [ "$(restarts)" = 0 ] && ! grep -q daemon-reload "$W/sd/calls"'
check "bad-config: the staged binary checks the staged config" 'grep -q "ERROR: bad (NEWBIN)" "$W/log"'

# ---- dropins -------------------------------------------------------------------------
world dropins; before=$(state)
printf 'stray\n' > "$W/stage/dropin.conf"
out=$(run FAKE_PEER=never HS2_SAFE_PEER_WAIT=10)
check "dropins: after a revert the drop-in directory is exactly what it was (files, modes, times, contents)" \
  'last | grep -q "^REVERTED" && [ "$(state)" = "$before" ]'
world nodir; rm -rf "$W/units/hs2-safetest.service.d"; before=$(state)
out=$(run FAKE_PEER=never HS2_SAFE_PEER_WAIT=10)
check "dropins: a unit that had none has none again after a revert" '[ ! -e "$W/units/hs2-safetest.service.d" ] && [ "$(state)" = "$before" ]'
world rmdrop; : > "$W/stage/dropin.conf"
out=$(run FAKE_PEER=0)
check "dropins: an empty staged dropin.conf removes the test drop-in (the others stay)" \
  'last | grep -q "^OK " && [ ! -e "$W/units/hs2-safetest.service.d/zz-test.conf" ] && [ -f "$W/units/hs2-safetest.service.d/10-limits.conf" ]'

# ---- the peer address -----------------------------------------------------------------
world mtcp; cfgjson mtcp 10.77.9.1 > "$W/stage/config.json"; before=$(state)
out=$(run FAKE_PEER=0)
check "mtcp (no tun): REFUSED unless HS2_SAFE_PEER says what to ping; nothing applied" \
  'last | grep -q "^REFUSED no-tun" && [ "$(state)" = "$before" ] && [ "$(restarts)" = 0 ]'
world mtcp2; cfgjson mtcp 10.77.9.1 > "$W/stage/config.json"
out=$(run FAKE_PEER=0 HS2_SAFE_PEER=192.0.2.77)
check "HS2_SAFE_PEER: pinged instead of peer_ip" 'last | grep -q "^OK " && [ "$(sort -u "$W/sd/pinged")" = 192.0.2.77 ]'
world newpeer; cfgjson l3mtcp 10.77.9.5 > "$W/stage/config.json"
out=$(run FAKE_PEER=0)
check "a staged config's peer_ip is the one pinged" '[ "$(sort -u "$W/sd/pinged")" = 10.77.9.5 ]'
world badunit
out=$(run FAKE_PEER=0 HS2_SAFE_UNIT=sshd)
check "a unit that is not an hs2 tunnel is refused" 'last | grep -q "^REFUSED" && [ "$(restarts)" = 0 ] && ! grep -q "restart" "$W/sd/calls" 2>/dev/null'

# ---- install.sh carries this script --------------------------------------------------
emb=$(awk '/<<'"'"'HS2_SAFE_APPLY_EOF'"'"'$/{f=1; next} /^HS2_SAFE_APPLY_EOF$/{f=0} f' "$INST")
check "install.sh carries scripts/hs2-safe-apply byte for byte" '[ -n "$emb" ] && [ "$emb" = "$(cat "$SA")" ]'
{ awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST"; } \
  | sed -e 's#^BIN=/usr/local/bin/hs2$#BIN="$SBX/bin/hs2"#' -e 's#^CFG_DIR=/etc/hs2$#CFG_DIR="$SBX/etc"#' \
        -e 's#^UNIT_DIR=/etc/systemd/system$#UNIT_DIR="$SBX/units"#' -e 's#^MENU_BIN=.*#MENU_BIN="$SBX/bin/hs2-menu"#' \
        -e 's#^SAFE_APPLY_BIN=.*#SAFE_APPLY_BIN="$SBX/sbin/hs2-safe-apply"#' -e 's#^BACKUP_DIR=.*#BACKUP_DIR="$SBX/bk"#' \
        -e 's#^\[ "\$(id -u)" = 0 \] || die .*#:#' > "$T/core.sh"
if grep -q 'SAFE_APPLY_BIN="$SBX/sbin/hs2-safe-apply"' "$T/core.sh"; then
  mkdir -p "$T/sbx/sbin" "$T/sbx/bin" "$T/sbx/etc" "$T/sbx/units"
  out=$(SBX="$T/sbx" ${JAILER:+"$JAILER"} bash -c 'source "'"$T/core.sh"'" >/dev/null 2>&1; install_safe_apply' 2>&1)
  check "install.sh installs it (mode 755) with the menu" \
    'cmp -s "$T/sbx/sbin/hs2-safe-apply" "$SA" && [ "$(stat -c %a "$T/sbx/sbin/hs2-safe-apply")" = 755 ] && sed -n "/^install_self(){/,/^}/p" "$INST" | grep -q install_safe_apply'
else
  check "install.sh names the installed path in SAFE_APPLY_BIN" 'false'
fi

exit $fail

#!/bin/bash
# zdu350 S8: install/zdu scripts (hs2-zdu-lib.sh, hs2-zdu-upgrade.sh, hs2-zdu-install.sh, hs2-safe-deploy.sh)
# with a fake systemctl/journalctl, a fake /proc entry for the running process and temp state dirs.
# The upgrade/install/safe-deploy runs use a copy of the lib whose root_owned_path only needs an absolute
# executable (the temp dir is not root-owned); root_owned_path itself is tested on its own.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); ZD="$HERE/../zdu"
T=$(mktemp -d); BG=""; trap 'for p in $BG; do kill $p 2>/dev/null; done; rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
. "$ZD/hs2-zdu-lib.sh"

# --- find_execstart_dropin
export SYSTEMD_DIR="$T/sd"; mkdir -p "$SYSTEMD_DIR/u1.service.d" "$SYSTEMD_DIR/u2.service.d" "$SYSTEMD_DIR/u3.service.d"
printf '[Service]\nEnvironment=A=1\n' > "$SYSTEMD_DIR/u1.service.d/20-env.conf"
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/hs2-x run -c /etc/hs2/c.json\n' > "$SYSTEMD_DIR/u1.service.d/40-tunnel1.conf"
cp "$SYSTEMD_DIR/u1.service.d/"*.conf "$SYSTEMD_DIR/u2.service.d/"
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/hs2-y run -c /etc/hs2/c.json\n' > "$SYSTEMD_DIR/u2.service.d/50-canary.conf"
printf '[Service]\nType=notify\n' > "$SYSTEMD_DIR/u2.service.d/60-other.conf"
printf '[Service]\nEnvironment=A=1\n' > "$SYSTEMD_DIR/u3.service.d/20-env.conf"
check "finder: t1 layout -> 40-tunnel1.conf (the ExecStart= reset line is not taken)" '[ "$(find_execstart_dropin u1)" = "$SYSTEMD_DIR/u1.service.d/40-tunnel1.conf" ]'
check "finder: t2 layout -> the last ExecStart drop-in 50-canary.conf, not a later one without ExecStart" '[ "$(find_execstart_dropin u2)" = "$SYSTEMD_DIR/u2.service.d/50-canary.conf" ]'
check "finder: env-only drop-ins -> empty" '[ -z "$(find_execstart_dropin u3)" ]'
check "finder: no drop-in dir -> empty" '[ -z "$(find_execstart_dropin nosuch)" ]'

# --- has_zdu
mkb(){ printf '#!/bin/bash\n%s\n' "$2" > "$T/$1"; chmod 0755 "$T/$1"; }
mkb zdu1 'echo "hs2 v3 [build x]"; [ "${2:-}" = --features ] && echo "features: zdu"; [ "${1:-}" = run ] && exec sleep 300; exit 0'
mkb zdu2 'echo "hs2 v3"; echo "features: foo zdu bar"'
mkb zdux 'echo "hs2 v3"; echo "features: zdux"'
mkb old 'echo "hs2 v3 [build rel69p]"; [ "${1:-}" = run ] && exec sleep 300; exit 0'
check "has_zdu: features: zdu" 'has_zdu "$T/zdu1"'
check "has_zdu: zdu among other features" 'has_zdu "$T/zdu2"'
check "has_zdu: zdux is not zdu" '! has_zdu "$T/zdux"'
check "has_zdu: pre-ZDU binary (ignores --features)" '! has_zdu "$T/old"'
check "has_zdu: missing binary" '! has_zdu "$T/nosuch"'

# --- root_owned_path
ln -s "$T/zdu1" "$T/link"
check "path: relative refused" '! root_owned_path zdu1'
check "path: symlink refused" '! root_owned_path "$T/link"'
check "path: file under a non-root or world-writable temp dir refused" '! root_owned_path "$T/zdu1"'
check "path: non-executable root file refused" '! root_owned_path /etc/hostname'
E=$(command -v env); [ -L "$E" ] && E=$(readlink -f "$E")
check "path: root-owned executable on a clean path ($E) accepted" 'root_owned_path "$E"'

# --- drain override guard (drill b1: HS2_ZDU_DRAIN=40 < 45 s probation cut every call at OK)
check "dur_s: 200 / 200s / 3m20s / 1h -> 200 200 200 3600" '[ "$(dur_s 200) $(dur_s 200s) $(dur_s 3m20s) $(dur_s 1h)" = "200 200 200 3600" ]'
check "dur_s: 1.5s / 500ms / -5 / x refused" '! dur_s 1.5s && ! dur_s 500ms && ! dur_s -5 && ! dur_s x'
check "drain: unset / 0 / 0s ok (HH:03 rule)" 'drain_override_ok "" && drain_override_ok 0 && drain_override_ok 0s'
check "drain: 40 (b1) and 104 refused, 105 / 200 / 20m ok" '! drain_override_ok 40 && ! drain_override_ok 104 && drain_override_ok 105 && drain_override_ok 200 && drain_override_ok 20m'
check "drain: probation 10s -> 70 ok, 69 refused; bad probation refused" 'drain_override_ok 70 10s && ! drain_override_ok 69 10s && ! drain_override_ok 200 1.5s'
check "drain: unparseable 1.5m refused" '! drain_override_ok 1.5m'

# --- scripts with a fake system (copy whose root_owned_path only needs an absolute executable)
mkdir -p "$T/z" "$T/bin" "$T/st" "$T/run" "$T/etc" "$T/lock" "$T/proc"
cp "$ZD"/*.sh "$T/z/"; echo 'root_owned_path(){ case "$1" in /*) [ -f "$1" ] && [ -x "$1" ] && [ ! -L "$1" ];; *) false;; esac; }' >> "$T/z/hs2-zdu-lib.sh"
cat > "$T/bin/systemctl" <<S
#!/bin/bash
echo "\$*" >> "$T/sd.log"
case "\$*" in
  "show -p MainPID --value"*) cat "$T/mainpid";;
  "show -p Environment --value"*) if [ -e "$T/unitenv" ]; then cat "$T/unitenv"; else echo HS2_ZDU=1; fi;;
  "show -p EnvironmentFiles --value"*) cat "$T/envfiles" 2>/dev/null;;
  "show -p NeedDaemonReload --value"*) [ -e "$T/needreload" ] && echo yes || echo no;;
  daemon-reload) rm -f "$T/needreload";;
  is-active*) [ -e "$T/inactive" ] && { echo inactive; exit 3; }
    n=\$(cat "$T/activating" 2>/dev/null || echo 0); [ "\$n" -gt 0 ] && { echo \$((n-1)) > "$T/activating"; echo activating; exit 3; }; echo active;;
esac
exit 0
S
printf '#!/bin/bash\ncat "%s/journal" 2>/dev/null\n' "$T" > "$T/bin/journalctl"; chmod 0755 "$T/bin/"*
export PATH="$T/bin:$PATH" HS2_ZDU_STATE_DIR="$T/st" HS2_ZDU_RUN_DIR="$T/run" HS2_ZDU_ETC_DIR="$T/etc" HS2_ZDU_LOCK_DIR="$T/lock" HS2_ZDU_PROC="$T/proc" HS2_SD_POLL=0 HS2_SD_STABLE=0 HS2_ZDU_UPGRADE_WAIT=20
bash -c 'trap "" USR2; exec sleep 300' & OP=$!; BG="$BG $OP"; echo $OP > "$T/mainpid"
mkdir -p "$T/proc/$OP"; ln -s "$T/zdu1" "$T/proc/$OP/exe"; printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
cp "$T/zdu1" "$T/new"; echo '# new build' >> "$T/new"
D="$SYSTEMD_DIR/hs2.service.d"; mkdir -p "$D"
reset_unit(){ rm -f "$D"/*; printf '[Service]\nEnvironment=HS2_NO_TUNE=1\n' > "$D/20-env.conf"
  printf '[Service]\nExecStart=\nExecStart=%s run -c /etc/hs2/c.json\n' "$1" > "$D/40-tunnel1.conf"
  [ "${2:-}" = zdu ] && { printf '[Service]\nType=notify\nNotifyAccess=all\nFileDescriptorStoreMax=32\n' > "$D/30-zdu.conf"; printf '[Service]\nEnvironment=HS2_ZDU=1\n' > "$D/31-zdu-env.conf"; }
  rm -f "$T/run/"* "$T/etc/"* "$T/journal" "$T/sd.log" "$T/inactive" "$T/unitenv" "$T/envfiles" "$T/needreload" "$T/activating"
  [ "${2:-}" = zdu ] && { mkdir -p "$T/run"; echo "ZDU-OK links=3 zdu=3" > "$T/run/hs2.peers"; }; : ; }
sha(){ sha256sum "$1" | cut -c1-16; }
# responder: when UNIT.next appears, write the handover states the old process would write
respond(){ ( for i in $(seq 1 100); do [ -e "$T/run/hs2.next" ] && break; sleep 0.1; done; sleep 0.7
  echo "PROBATION 4242" > "$T/run/hs2.handover"; sleep 0.6; echo "$1" > "$T/run/hs2.handover" ) & BG="$BG $!"; }
UP(){ bash "$T/z/hs2-zdu-upgrade.sh" hs2 "$1" "$(sha "$1")" > "$T/out" 2>&1; echo $?; }

reset_unit "$T/zdu1" zdu
for m in 0 3; do
  rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=$m UP "$T/new")
  check "upgrade: minute $m refused (exit 4, REFUSED, no UNIT.next, drop-in unchanged)" '[ "$rc" = 4 ] && grep -q "^REFUSED window" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ] && grep -q "^ExecStart=$T/zdu1 " "$D/40-tunnel1.conf"'
done
# end of hour: the window opens within 2 min (READY + probation would run into it)
reset_unit "$T/zdu1" zdu
for m in 58 59; do
  rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=$m UP "$T/new")
  check "upgrade: minute $m refused (window opens in <= 2 min), no UNIT.next" '[ "$rc" = 4 ] && grep -q "^REFUSED window opens in" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
done
# HS2_ZDU_STORM_MIN=31 in the running process: window HH:30-HH:33, HH:00-HH:03 allowed
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_STORM_MIN=31\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=32 UP "$T/new")
check "upgrade: storm minute 31 -> minute 32 refused" '[ "$rc" = 4 ] && grep -q "^REFUSED window HH:30-HH:33" "$T/st/hs2-zdu-upgrade.hs2.out"'
reset_unit "$T/zdu1" zdu; rm -f "$T/run/hs2.peers"; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=1 UP "$T/new")
check "upgrade: storm minute 31 -> minute 1 passes the window (stops at the peers gate)" '[ "$rc" = 4 ] && grep -q "^REFUSED no .*hs2.peers" "$T/st/hs2-zdu-upgrade.hs2.out"'
printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
# review nit: STORM_MIN read like the process (base 10, trimmed): "08" -> HH:07-HH:10, " 5" -> HH:04-HH:07
check "storm_min: 08 -> 8, 09 -> 9, ' 5' -> 5, '' -> 1, 60 -> 1, x -> 1" '[ "$(storm_min 08)" = 8 ] && [ "$(storm_min 09)" = 9 ] && [ "$(storm_min " 5")" = 5 ] && [ "$(storm_min "")" = 1 ] && [ "$(storm_min 60)" = 1 ] && [ "$(storm_min x)" = 1 ]'
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_STORM_MIN=08\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=9 UP "$T/new")
check "upgrade: storm minute 08 -> minute 9 refused HH:07-HH:10 (no octal error)" '[ "$rc" = 4 ] && grep -q "^REFUSED window HH:07-HH:10" "$T/st/hs2-zdu-upgrade.hs2.out" && ! grep -q "value too great" "$T/out"'
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_STORM_MIN= 5\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=6 UP "$T/new")
check "upgrade: storm minute ' 5' -> minute 6 refused HH:04-HH:07" '[ "$rc" = 4 ] && grep -q "^REFUSED window HH:04-HH:07" "$T/st/hs2-zdu-upgrade.hs2.out"'
printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
# review H1: direct (non-reverse) mode: the process writes ZDU-DIRECT; the script refuses with a clear message
reset_unit "$T/zdu1" zdu; echo "ZDU-DIRECT mode=direct: live handover needs reverse mode" > "$T/run/hs2.peers"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: direct mode (ZDU-DIRECT) -> REFUSED 4 direct, no UNIT.next" '[ "$rc" = 4 ] && grep -q "^REFUSED direct (non-reverse) mode" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
# review 4 M1: a second upgrade while the previous handover's old process still drains is refused
reset_unit "$T/zdu1" zdu; echo "OK $OP" > "$T/run/hs2.handover"; printf 'Name:\ths2\nPPid:\t31337\n' > "$T/proc/$OP/status"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: old process still draining (OK + MainPID PPid 31337) -> REFUSED 4 drain running, no UNIT.next" '[ "$rc" = 4 ] && grep -q "^REFUSED drain running: .*pid 31337" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
reset_unit "$T/zdu1" zdu; echo "DRAIN-END $OP" > "$T/run/hs2.handover"; printf 'Name:\ths2\nPPid:\t31337\n' > "$T/proc/$OP/status"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 HS2_ZDU_UPGRADE_WAIT=1 UP "$T/new")
check "upgrade: DRAIN-END -> passes the drain gate" '! grep -q "^REFUSED drain running" "$T/st/hs2-zdu-upgrade.hs2.out"'
reset_unit "$T/zdu1" zdu; echo "OK $OP" > "$T/run/hs2.handover"; printf 'Name:\ths2\nPPid:\t1\n' > "$T/proc/$OP/status"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 HS2_ZDU_UPGRADE_WAIT=1 UP "$T/new")
check "upgrade: stale OK but the old exited (PPid 1) -> passes the drain gate" '! grep -q "^REFUSED drain running" "$T/st/hs2-zdu-upgrade.hs2.out"'
rm -f "$T/proc/$OP/status" "$T/st/hs2-zdu-upgrade.log" # the passing cases sent SIGUSR2 (trapped)
# review nit: dropin_env honours EnvironmentFile= (a file value overrides Environment=)
mkdir -p "$T/de"; printf '[Service]\nEnvironment=HS2_ZDU_DRAIN=30m HS2_X=1\n' > "$T/de/20-env.conf"
check "dropin_env: Environment= only" '[ "$(dropin_env "$T/de" HS2_ZDU_DRAIN)" = 30m ]'
printf 'HS2_ZDU_DRAIN="40s"\nexport HS2_Y=2\n' > "$T/de.env"; printf '[Service]\nEnvironmentFile=-%s\nEnvironmentFile=-/nonexistent\n' "$T/de.env" > "$T/de/25-file.conf"
check "dropin_env: EnvironmentFile= value overrides (quotes stripped, missing '-' file ignored)" '[ "$(dropin_env "$T/de" HS2_ZDU_DRAIN)" = 40s ] && [ "$(dropin_env "$T/de" HS2_Y)" = 2 ]'
# review F2: the running process's peer check (UNIT.peers) gates the SIGUSR2
reset_unit "$T/zdu1" zdu; rm -f "$T/run/hs2.peers"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: no UNIT.peers -> REFUSED 4, no UNIT.next, no SIGUSR2" '[ "$rc" = 4 ] && grep -q "^REFUSED no .*hs2.peers" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ] && ! grep -q SIGUSR2 "$T/st/hs2-zdu-upgrade.log" 2>/dev/null'
reset_unit "$T/zdu1" zdu; echo "ZDU-NO links=200 zdu=150" > "$T/run/hs2.peers"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: a peer without ZDU (ZDU-NO) -> REFUSED 4, no UNIT.next" '[ "$rc" = 4 ] && grep -q "^REFUSED peers: ZDU-NO links=200 zdu=150" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
reset_unit "$T/zdu1" zdu; touch -d "@$(( $(date +%s) - 120 ))" "$T/run/hs2.peers"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: stale UNIT.peers (120 s) -> REFUSED 4" '[ "$rc" = 4 ] && grep -q "hs2.peers is .*s old (stale)" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
reset_unit "$T/zdu1" zdu; respond "REVERTED child exited in probation"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=4 UP "$T/new")
check "upgrade: minute 4 not refused; REVERTED -> exit 2, REVERTED line" '[ "$rc" = 2 ] && grep -q "^REVERTED (REVERTED child exited" "$T/st/hs2-zdu-upgrade.hs2.out"'
check "upgrade REVERTED: drop-in unchanged, no last-good, UNIT.next removed, no daemon-reload" 'grep -q "^ExecStart=$T/zdu1 " "$D/40-tunnel1.conf" && [ ! -e "$T/etc/last-good.hs2.conf" ] && [ ! -e "$T/run/hs2.next" ] && ! grep -q daemon-reload "$T/sd.log"'
check "upgrade: the old process got SIGUSR2 and lives (it ignores it here)" 'kill -0 $OP'
reset_unit "$T/zdu1" zdu; respond "OK 4242"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade OK: exit 0, ExecStart switched, env drop-in untouched, last-good = new, daemon-reload" '[ "$rc" = 0 ] && grep -q "^ExecStart=$T/new run -c /etc/hs2/c.json" "$D/40-tunnel1.conf" && grep -q HS2_NO_TUNE "$D/20-env.conf" && grep -q "^ExecStart=$T/new " "$T/etc/last-good.hs2.conf" && grep -qx daemon-reload "$T/sd.log"'
check "upgrade OK: UNIT.next written with the new path, then removed" 'grep -q "upgrade .* -> $T/new: SIGUSR2" "$T/st/hs2-zdu-upgrade.log" && [ ! -e "$T/run/hs2.next" ]'
# zdu389: a drop-in Environment change only applies at a cold restart -> refuse the live handover
reset_unit "$T/zdu1" zdu; echo 'HS2_ZDU=1 HS2_FOO=2' > "$T/unitenv"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: unit env adds HS2_FOO=2 -> REFUSED 4 env changed, no UNIT.next" '[ "$rc" = 4 ] && grep -q "^REFUSED env changed: +HS2_FOO=2 .*cold restart" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
reset_unit "$T/zdu1" zdu; echo '' > "$T/unitenv"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: unit env drops HS2_ZDU=1 -> REFUSED 4 (-HS2_ZDU=1)" '[ "$rc" = 4 ] && grep -q "^REFUSED env changed: -HS2_ZDU=1" "$T/st/hs2-zdu-upgrade.hs2.out"'
reset_unit "$T/zdu1" zdu; echo 'HS2_ZDU=1 "HS2_X=a b"' > "$T/unitenv"; printf 'export HS2_BAR="v1"\nOTHER=1\n' > "$T/envf"; echo "$T/envf (ignore_errors=no)" > "$T/envfiles"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: EnvironmentFile + quoted value -> REFUSED with +HS2_BAR=v1 +HS2_X=a b" '[ "$rc" = 4 ] && grep -q "+HS2_BAR=v1 +HS2_X=a b" "$T/st/hs2-zdu-upgrade.hs2.out"'
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_HANDOVER_FD=3\0HS2_UNIT=hs2\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; echo 'HS2_UNIT=hs2 HS2_ZDU=1' > "$T/unitenv"; touch "$T/needreload"; respond "OK 4242"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: same env (HS2_HANDOVER_FD ignored), unit changed on disk -> daemon-reload first, OK" '[ "$rc" = 0 ] && [ "$(grep -n daemon-reload "$T/sd.log" | head -1 | cut -d: -f1)" -lt "$(grep -n "show -p Environment " "$T/sd.log" | head -1 | cut -d: -f1)" ]'
printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; echo "zdu: handover refused: busy (draining)" > "$T/journal"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: refusal logged by the process -> exit 4, drop-in unchanged" '[ "$rc" = 4 ] && grep -q "^REFUSED by pid" "$T/st/hs2-zdu-upgrade.hs2.out" && grep -q "^ExecStart=$T/zdu1 " "$D/40-tunnel1.conf"'
reset_unit "$T/zdu1" zdu
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/old"); check "upgrade: new binary without zdu -> ABORT 1" '[ "$rc" = 1 ] && grep -q "lacks the zdu feature" "$T/out"'
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$T/z/hs2-zdu-upgrade.sh" hs2 "$T/new" 0000000000 >"$T/out" 2>&1; echo $?); check "upgrade: sha mismatch -> ABORT 1" '[ "$rc" = 1 ] && grep -q sha-mismatch "$T/out"'
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$T/z/hs2-zdu-upgrade.sh" hs2 new "$(sha "$T/new")" >"$T/out" 2>&1; echo $?); check "upgrade: relative path -> ABORT 1" '[ "$rc" = 1 ] && grep -q "root-owned" "$T/out"'
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$ZD/hs2-zdu-upgrade.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?); check "upgrade (real lib): binary under a temp dir -> ABORT 1 (path check)" '[ "$rc" = 1 ] && grep -q "root-owned" "$T/out"'
rm -f "$D/30-zdu.conf"; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: no 30-zdu.conf -> ABORT 1" '[ "$rc" = 1 ] && grep -q "30-zdu.conf missing" "$T/out"'
reset_unit "$T/zdu1" zdu; printf 'PATH=/bin\0' > "$T/proc/$OP/environ"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: running process without HS2_ZDU=1 -> ABORT 1" '[ "$rc" = 1 ] && grep -q "without HS2_ZDU=1" "$T/out"'
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_DRAIN=40\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: running process with HS2_ZDU_DRAIN=40 -> ABORT 1, no UNIT.next" '[ "$rc" = 1 ] && grep -q "HS2_ZDU_DRAIN=40: below probation" "$T/out" && [ ! -e "$T/run/hs2.next" ]'
printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_DRAIN=200\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; printf '[Service]\nEnvironment=HS2_ZDU_DRAIN=90s\n' > "$D/21-drain.conf"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: process drain 200 ok but a drop-in sets 90s -> ABORT 1" '[ "$rc" = 1 ] && grep -q "drop-in of hs2 sets HS2_ZDU_DRAIN=90s" "$T/out"'
rm -f "$D/30-zdu.conf" "$D/31-zdu-env.conf"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$T/z/hs2-zdu-install.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "install: a drop-in with HS2_ZDU_DRAIN=90s -> ABORT, no ZDU drop-ins written" '[ "$rc" = 1 ] && grep -q "HS2_ZDU_DRAIN=90s" "$T/out" && [ ! -e "$D/31-zdu-env.conf" ]'
printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
reset_unit "$T/new" zdu; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: same binary -> ALREADY 0" '[ "$rc" = 0 ] && grep -q "^ALREADY" "$T/st/hs2-zdu-upgrade.hs2.out"'
rm -f "$D"/*; printf '[Service]\nEnvironment=A=1\n' > "$D/20-env.conf"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new"); check "upgrade: no ExecStart drop-in -> ABORT 1" '[ "$rc" = 1 ] && grep -q "no ExecStart drop-in" "$T/out"'

# --- hs2-zdu-install.sh
reset_unit "$T/zdu1"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$T/z/hs2-zdu-install.sh" hs2 "$T/zdu1" "$(sha "$T/zdu1")" >"$T/out" 2>&1; echo $?)
check "install: same path as the running ExecStart -> ABORT, no ZDU drop-ins" '[ "$rc" = 1 ] && grep -q "already the ExecStart" "$T/out" && [ ! -e "$D/30-zdu.conf" ] && [ ! -e "$D/31-zdu-env.conf" ]'
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=2 bash "$T/z/hs2-zdu-install.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "install: minute 2 refused (exit 4), no ZDU drop-ins" '[ "$rc" = 4 ] && [ ! -e "$D/30-zdu.conf" ]'
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 bash "$T/z/hs2-zdu-install.sh" hs2 "$T/old" "$(sha "$T/old")" >"$T/out" 2>&1; echo $?)
check "install: binary without zdu -> ABORT" '[ "$rc" = 1 ] && grep -q "lacks zdu" "$T/out"'

# --- hs2-safe-deploy.sh R3. The health check reads /proc/MainPID/cmdline: a real process named after the binary.
bash -c "exec -a '$T/old' sleep 300" & HP=$!; BG="$BG $HP"
reset_unit "$T/zdu1" zdu; echo $HP > "$T/mainpid"
rc=$(bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/old" "$(sha "$T/old")" >"$T/out" 2>&1; echo $?)
check "safe-deploy rollback to a pre-ZDU binary: OK, 30-zdu.conf + 31-zdu-env.conf removed before the restart" '[ "$rc" = 0 ] && [ ! -e "$D/30-zdu.conf" ] && [ ! -e "$D/31-zdu-env.conf" ] && grep -q "R3: .* removed 30-zdu.conf + 31-zdu-env.conf" "$T/st/hs2-safe-deploy.log" && grep -q "^ExecStart=$T/old " "$D/40-tunnel1.conf"'
check "safe-deploy: removal precedes daemon-reload + restart" 'grep -q "daemon-reload" "$T/sd.log" && [ "$(grep -n "^restart --no-block hs2" "$T/sd.log" | cut -d: -f1)" -gt "$(grep -n "^daemon-reload" "$T/sd.log" | head -1 | cut -d: -f1)" ]'
reset_unit "$T/zdu1" zdu; echo $OP > "$T/mainpid"   # health check fails (cmdline is sleep) -> revert to zdu1
rc=$(bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/old" "$(sha "$T/old")" >"$T/out" 2>&1; echo $?)
check "safe-deploy failed rollback: REVERTED (2), old ExecStart and both ZDU drop-ins restored" '[ "$rc" = 2 ] && grep -q "^ExecStart=$T/zdu1 " "$D/40-tunnel1.conf" && [ -e "$D/30-zdu.conf" ] && grep -q HS2_ZDU=1 "$D/31-zdu-env.conf"'
reset_unit "$T/zdu1"; echo $OP > "$T/mainpid"
rc=$(bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "safe-deploy without ZDU drop-ins: no R3 action" '[ "$rc" = 2 ] && ! grep -q "R3:" "$T/out"'
# zdu389: non-blocking restart; "activating" (Type=notify before READY=1) is polled, not waited on in systemctl
bash -c "exec -a '$T/new' sleep 300" & HN=$!; BG="$BG $HN"
reset_unit "$T/zdu1" zdu; echo $HN > "$T/mainpid"; echo 7 > "$T/activating"
rc=$(bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "safe-deploy: restart --no-block, 7 polls activating then active -> OK" '[ "$rc" = 0 ] && grep -qx "restart --no-block hs2" "$T/sd.log" && ! grep -qx "restart hs2" "$T/sd.log" && [ "$(grep -c "^is-active" "$T/sd.log")" -ge 8 ]'
reset_unit "$T/zdu1" zdu; echo $HN > "$T/mainpid"; echo 1000 > "$T/activating"
rc=$(HS2_SD_TRIES=5 bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "safe-deploy: never READY (activating) -> bounded poll, REVERTED, revert restart --no-block too" '[ "$rc" = 3 ] && [ "$(grep -c "^restart --no-block hs2" "$T/sd.log")" = 2 ] && grep -q "^ExecStart=$T/zdu1 " "$D/40-tunnel1.conf"'
check "install: 30-zdu.conf sets TimeoutStartSec=90" 'grep -q "TimeoutStartSec=90.*30-zdu.conf" "$T/z/hs2-zdu-install.sh"'
exit $fail

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

# --- b430 prune_backups (real lib): keep newest HS2_BACKUP_KEEP (10) + younger than 7 days, exact names only
PB="$T/pb"; NOWS=$(date +%s)
mkbk(){ local d="$PB/hs2-backup-$(date -d "@$((NOWS - $2*86400 - ${4:-0}))" +%Y%m%d-%H%M%S)-pre-$3-$1"; mkdir -p "$d"; touch -d "@$((NOWS - $2*86400))" "$d"; echo "$d"; }
nbk(){ ls -1A "$PB" | grep -c -- "-pre-$2-$1\$"; }
mkdir -p "$PB"; for i in $(seq 20 31); do mkbk hs2 $i zdu > /dev/null; done; for i in 1 2 3; do mkbk hs2 $i zdu > /dev/null; done
OLDEST=$(date -d "@$((NOWS-31*86400))" +%Y%m%d)
check "prune: 12 old + 3 recent -> the newest 10 stay (3 recent + 7 old)" 'prune_backups "$PB" hs2 zdu >/dev/null; [ "$(nbk hs2 zdu)" = 10 ] && ! ls -1A "$PB" | grep -q "hs2-backup-$OLDEST"'
rm -rf "$PB"; mkdir -p "$PB"; for i in $(seq 0 11); do mkbk hs2 0 zdu $((i*3600)) > /dev/null; done; mkbk hs2 40 zdu > /dev/null; mkbk hs2 41 zdu > /dev/null
check "prune: 12 younger than 7 days + 2 old -> the 12 young stay (union), the 2 old go" 'prune_backups "$PB" hs2 zdu >/dev/null; [ "$(nbk hs2 zdu)" = 12 ]'
rm -rf "$PB"; mkdir -p "$PB" "$T/etc"
for i in $(seq 20 33); do mkbk hs2 $i zdu > /dev/null; mkbk x-hs2 $i zdu > /dev/null; mkbk hs2-x $i zdu > /dev/null; mkbk hs2 $i safe > /dev/null; done
OLD1=$(mkbk hs2 50 zdu); OLD2=$(mkbk hs2 51 zdu); OLD3=$(mkbk hs2 52 zdu); OLD4=$(mkbk hs2 53 zdu)
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/hs2-a run\n' > "$T/etc/last-good.hs2.conf"
mkdir -p "$OLD1/hs2.service.d"; cp "$T/etc/last-good.hs2.conf" "$OLD1/hs2.service.d/40-tunnel1.conf"; touch -d "@$((NOWS-50*86400))" "$OLD1"
echo "OK /x pid=1 (40-tunnel1.conf ExecStart switched, last-good recorded, backup $OLD2)" > "$PB/hs2-zdu-upgrade.hs2.out"
B=$(basename "$OLD3"); mkdir -p "$PB/${B}x" "$PB/${B/hs2-backup-2/hs2-backup-x}"; touch "$PB/${B%hs2}hs2y"
F=$(basename "$(mkbk hs2 60 zdu)"); rm -rf "${PB:?}/$F"; touch -d "@$((NOWS-60*86400))" "$PB/$F"   # a file, not a directory
L=$(basename "$(mkbk hs2 61 zdu)"); rm -rf "${PB:?}/$L"; mkdir -p "$T/linktarget"; ln -s "$T/linktarget" "$PB/$L"
mkdir -p "$PB/hs2-backup-20200101-000000-pre-zdu-hs2.d" "$PB/hs2-backup-20200101-000000-pre-zdu-hs2 x" "$PB/hs2-backup-20200101-0000000-pre-zdu-hs2"
for d in "$PB"/*; do [ -L "$d" ] || touch -d "@$((NOWS-90*86400))" "$d"; done; touch "$PB/$B" 2>/dev/null; touch -d "@$((NOWS-52*86400))" "$PB/$B"
OUTP=$(HS2_ZDU_ETC_DIR="$T/etc" prune_backups "$PB" hs2 zdu "$OLD4")
# hs2 zdu: 14 (20..33 days) + OLD1..4 + file + symlink = 20 names; 22 names match the suffix (incl. near misses); 5 removed -> 17
check "prune: hs2 zdu -> 10 newest + last-good copy + .out-named + KEEP_DIR (+ the file and the symlink) left" '[ "$(nbk hs2 zdu)" = 17 ] && [ -d "$OLD1" ] && [ -d "$OLD2" ] && [ -d "$OLD4" ] && [ ! -e "$OLD3" ]'
check "prune: one line per removal (5), all exact hs2 zdu names" '[ "$(echo "$OUTP" | grep -c "^pruned ")" = 5 ] && ! echo "$OUTP" | grep -v -- "-pre-zdu-hs2$" | grep -q .'
check "prune: other units (x-hs2, hs2-x) and the safe kind untouched" '[ "$(nbk x-hs2 zdu)" = 14 ] && [ "$(nbk hs2-x zdu)" = 14 ] && [ "$(nbk hs2 safe)" = 14 ]'
check "prune: near-miss names, a plain file and a symlink (and its target) untouched" '[ -d "$PB/${B}x" ] && [ -d "$PB/${B/hs2-backup-2/hs2-backup-x}" ] && [ -e "$PB/${B%hs2}hs2y" ] && [ -f "$PB/$F" ] && [ -L "$PB/$L" ] && [ -d "$T/linktarget" ] && [ -d "$PB/hs2-backup-20200101-000000-pre-zdu-hs2.d" ] && [ -d "$PB/hs2-backup-20200101-000000-pre-zdu-hs2 x" ] && [ -d "$PB/hs2-backup-20200101-0000000-pre-zdu-hs2" ]'
prune_backups "$PB" hs2 safe >/dev/null
check "prune: safe kind on its own -> 10 left" '[ "$(nbk hs2 safe)" = 10 ] && [ "$(nbk x-hs2 zdu)" = 14 ]'
N0=$(ls -1A "$PB" | wc -l)
prune_backups "pb" hs2 zdu; prune_backups "$PB" "hs2*" zdu; prune_backups "$PB" "" zdu; prune_backups "$PB" hs2 "zdu|safe"; prune_backups / hs2 zdu; prune_backups "$PB" "x/../hs2" zdu
HS2_BACKUP_KEEP=x prune_backups "$PB" x-hs2 zdu
check "prune: relative or / state dir, glob/empty/path unit, bad kind, bad keep -> no-op" '[ "$(ls -1A "$PB" | wc -l)" = "$N0" ]'
HS2_BACKUP_KEEP=0 HS2_BACKUP_DAYS=0 prune_backups "$PB" x-hs2 zdu >/dev/null
check "prune: keep 0 days 0 -> every x-hs2 zdu backup goes, hs2 / hs2-x untouched" '[ "$(nbk x-hs2 zdu)" = 0 ] && [ "$(nbk hs2-x zdu)" = 14 ] && [ "$(nbk hs2 zdu)" = 17 ]'
rm -rf "$PB" "$T/etc/last-good.hs2.conf" "$T/linktarget"

# --- b430 zdu_window_sh (the rule of an older running binary; the process's own: hs2 zdu-window)
H0=$(( NOWS / 3600 * 3600 ))
wv(){ zdu_window_sh $((H0 + $1)) "$2" "$3" "$4"; }
check "window sh: HH:57:29 slack 0 OK, HH:57:30 refused (90+45+15 = 150 s before HH:00)" '[ "$(wv 3449 "" "" 0 | cut -c1-12)" = "OK span=150s" ] && [ "$(wv 3450 "" "" 0 | cut -c1-17)" = "window opens in <" ]'
check "window sh: slack 10 -> HH:57:19 OK, HH:57:20 refused" '[ "$(wv 3439 "" "" 10 | cut -c1-2)" = OK ] && [ "$(wv 3440 "" "" 10 | cut -c1-6)" = window ]'
check "window sh: HH:00:00 / HH:03:59 in the window, HH:04:00 OK" '[ "$(wv 0 "" "" 0 | cut -c1-12)" = "window HH:00" ] && [ "$(wv 239 "" "" 0 | cut -c1-12)" = "window HH:00" ] && [ "$(wv 240 "" "" 0 | cut -c1-2)" = OK ]'
check "window sh: probation 10s -> span 115 s: HH:58:04 OK, HH:58:05 refused" '[ "$(wv 3484 "" 10s 0 | cut -c1-2)" = OK ] && [ "$(wv 3485 "" 10s 0 | cut -c1-6)" = window ]'
check "window sh: storm minute 31 -> HH:30:00 refused, HH:27:29 OK, HH:27:30 refused" '[ "$(wv 1800 31 "" 0 | cut -c1-12)" = "window HH:30" ] && [ "$(wv 1649 31 "" 0 | cut -c1-2)" = OK ] && [ "$(wv 1650 31 "" 0 | cut -c1-6)" = window ]'
if [ -n "${HS2_TEST_BIN:-}" ] && [ -x "${HS2_TEST_BIN:-}" ]; then
  # one rule: the real binary and the shell fallback agree on every 10 s of an hour (several storm/probation/slack)
  bad=0
  for c in "::0" "::10" "31::10" ":10s:10" "8:1m:0" "0::10"; do IFS=: read -r sm pr sl <<< "$c"
    for o in $(seq 0 10 3590) 3595 3599 3439 3440 3449 3450; do
      a=$(HS2_ZDU_STORM_MIN="$sm" HS2_ZDU_PROBATION="$pr" "$HS2_TEST_BIN" zdu-window $((H0+o)) "$sl" | cut -c1-2)
      b=$(zdu_window_sh $((H0+o)) "$sm" "$pr" "$sl" | cut -c1-2)
      [ "$a" = "$b" ] || { bad=$((bad+1)); echo "  window mismatch o=$o $c bin=$a sh=$b"; }
    done; done
  check "window: real binary ($HS2_TEST_BIN) == shell fallback on every 10 s of the hour (6 settings)" '[ "$bad" = 0 ]'
  check "window: real binary lists zdu and zduwin" 'has_feature "$HS2_TEST_BIN" zduwin && has_zdu "$HS2_TEST_BIN"'
fi

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
export PATH="$T/bin:$PATH" HS2_ZDU_STATE_DIR="$T/st" HS2_ZDU_RUN_DIR="$T/run" HS2_ZDU_ETC_DIR="$T/etc" HS2_ZDU_LOCK_DIR="$T/lock" HS2_ZDU_PROC="$T/proc" HS2_SD_POLL=0 HS2_SD_STABLE=0 HS2_ZDU_UPGRADE_WAIT=20 HS2_ZDU_PEER_RETRY=1
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
check "upgrade b443: ZDU-NO is re-checked once after HS2_ZDU_PEER_RETRY (logged, named in the REFUSED line)" 'grep -q "peers: ZDU-NO links=200 zdu=150: waiting 1s once" "$T/st/hs2-zdu-upgrade.log" && grep -q "re-checked after 1s" "$T/st/hs2-zdu-upgrade.hs2.out"'
# b443 (lab r443 op3): a link just up has not announced ZDU yet; the next peers file is ZDU-OK -> the handover goes on
reset_unit "$T/zdu1" zdu; echo "ZDU-NO links=194 zdu=193" > "$T/run/hs2.peers"; ( sleep 0.3; echo "ZDU-OK links=194 zdu=194" > "$T/run/hs2.peers" ) & BG="$BG $!"
respond "OK 4242"; rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade b443: ZDU-NO that turns ZDU-OK within the wait -> OK (exit 0)" '[ "$rc" = 0 ] && grep -q "after the wait: going on" "$T/st/hs2-zdu-upgrade.log" && grep -q "^OK " "$T/st/hs2-zdu-upgrade.hs2.out"'
# b443: the process refuses at SIGUSR2 with peers ZDU-NO -> one SIGUSR2 retry after the wait, then REFUSED
reset_unit "$T/zdu1" zdu; rm -f "$T/st/hs2-zdu-upgrade.log"; echo "zdu: handover refused: peers: ZDU-NO links=194 zdu=193: a peer without ZDU" > "$T/journal"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade b443: process ZDU-NO refusal -> one SIGUSR2 retry, then REFUSED 4, no UNIT.next" '[ "$rc" = 4 ] && [ "$(grep -c ": SIGUSR2" "$T/st/hs2-zdu-upgrade.log")" = 2 ] && grep -q "SIGUSR2 (retry)" "$T/st/hs2-zdu-upgrade.log" && grep -q "^REFUSED by pid" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
reset_unit "$T/zdu1" zdu; echo "zdu: handover refused: drain running" > "$T/journal"; rm -f "$T/st/hs2-zdu-upgrade.log"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade b443: any other process refusal -> no retry (one SIGUSR2)" '[ "$rc" = 4 ] && [ "$(grep -c ": SIGUSR2" "$T/st/hs2-zdu-upgrade.log")" = 1 ]'
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
# --- b430: the window as the script asks it (fake binaries: zdu1 has no zduwin -> shell rule; zduw answers itself)
echo $OP > "$T/mainpid"; reset_unit "$T/zdu1" zdu; rm -f "$T/run/hs2.peers"
rc=$(HS2_ZDU_UPGRADE_FAKE_NOW=$((H0+3439)) UP "$T/new")
check "upgrade (older binary): HH:57:19 passes the window (stops at the peers gate)" '[ "$rc" = 4 ] && grep -q "^REFUSED no .*hs2.peers" "$T/st/hs2-zdu-upgrade.hs2.out"'
rc=$(HS2_ZDU_UPGRADE_FAKE_NOW=$((H0+3440)) UP "$T/new")
check "upgrade (older binary): HH:57:20 refused (150 s span + 10 s slack; the process refuses from HH:57:30)" '[ "$rc" = 4 ] && grep -q "^REFUSED window opens in <= 160s (minute 57)" "$T/st/hs2-zdu-upgrade.hs2.out" && [ ! -e "$T/run/hs2.next" ]'
mkb zduw 'case "${1:-}" in version) echo "hs2 v3"; [ "${2:-}" = --features ] && echo "features: zdu zduwin";; zdu-window) echo "window from-binary now=$2 slack=$3 sm=$HS2_ZDU_STORM_MIN pr=$HS2_ZDU_PROBATION"; exit 4;; run) exec sleep 300;; esac; exit 0'
rm -f "$T/proc/$OP/exe"; ln -s "$T/zduw" "$T/proc/$OP/exe"; printf 'PATH=/bin\0HS2_ZDU=1\0HS2_ZDU_STORM_MIN=7\0HS2_ZDU_PROBATION=20s\0' > "$T/proc/$OP/environ"
reset_unit "$T/zdu1" zdu; echo 'HS2_ZDU=1 HS2_ZDU_STORM_MIN=7 HS2_ZDU_PROBATION=20s' > "$T/unitenv"; rc=$(HS2_ZDU_UPGRADE_FAKE_NOW=$((H0+1800)) UP "$T/new")
check "upgrade (zduwin binary): the running binary decides, with the process's storm minute + probation, slack 10" '[ "$rc" = 4 ] && grep -q "^REFUSED window from-binary now=$((H0+1800)) slack=10 sm=7 pr=20s" "$T/st/hs2-zdu-upgrade.hs2.out"'
rm -f "$T/proc/$OP/exe"; ln -s "$T/zdu1" "$T/proc/$OP/exe"; printf 'PATH=/bin\0HS2_ZDU=1\0' > "$T/proc/$OP/environ"
# --- b430: a refused attempt keeps the previous UNIT.handover marker
reset_unit "$T/zdu1" zdu; echo "DRAIN-END 999 drained 12 links" > "$T/run/hs2.handover"; echo "zdu: handover refused: busy (draining)" > "$T/journal"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: refused by the process -> the previous DRAIN-END marker is still there" '[ "$rc" = 4 ] && grep -q "^REFUSED by pid" "$T/st/hs2-zdu-upgrade.hs2.out" && [ "$(cat "$T/run/hs2.handover")" = "DRAIN-END 999 drained 12 links" ]'
reset_unit "$T/zdu1" zdu; echo "DRAIN-END 999" > "$T/run/hs2.handover"; respond "OK 4242"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: previous DRAIN-END marker, then PROBATION + OK written -> OK 0 (the old marker is not taken as a state)" '[ "$rc" = 0 ] && grep -q "handover: PROBATION 4242" "$T/st/hs2-zdu-upgrade.log"'
reset_unit "$T/zdu1" zdu; echo "OK 777" > "$T/run/hs2.handover"; printf 'Name:\ths2\nPPid:\t1\n' > "$T/proc/$OP/status"; echo "zdu: handover refused: x" > "$T/journal"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade: a previous OK marker is not this attempt's OK (refusal seen, exit 4, marker kept)" '[ "$rc" = 4 ] && grep -q "^REFUSED by pid" "$T/st/hs2-zdu-upgrade.hs2.out" && [ "$(cat "$T/run/hs2.handover")" = "OK 777" ]'
# --- b430: an OK upgrade / safe deploy prunes that unit's old backups of its own kind
rm -rf "$T/st"/hs2-backup-*
for i in $(seq 20 31); do d="$T/st/hs2-backup-$(date -d "@$((NOWS-i*86400))" +%Y%m%d-%H%M%S)-pre-zdu-hs2"; mkdir -p "$d"; touch -d "@$((NOWS-i*86400))" "$d"
  d2="${d%zdu-hs2}safe-hs2"; mkdir -p "$d2"; touch -d "@$((NOWS-i*86400))" "$d2"; done
reset_unit "$T/zdu1" zdu; respond "OK 4242"; rm -f "$T/st/hs2-zdu-upgrade.hs2.out"
rc=$(HS2_ZDU_UPGRADE_FAKE_MIN=30 UP "$T/new")
check "upgrade OK: 12 old zdu backups + the new one -> 10 left (new one kept), safe ones untouched, 3 prunes logged" '[ "$rc" = 0 ] && [ "$(ls -1d "$T/st"/hs2-backup-*-pre-zdu-hs2 | wc -l)" = 10 ] && [ -d "$(grep -o "backup /[^)]*" "$T/st/hs2-zdu-upgrade.hs2.out" | cut -d" " -f2)" ] && [ "$(ls -1d "$T/st"/hs2-backup-*-pre-safe-hs2 | wc -l)" = 12 ] && [ "$(grep -c "pruned $T/st/hs2-backup-" "$T/st/hs2-zdu-upgrade.log")" = 3 ]'
bash -c "exec -a '$T/new' sleep 300" & HN2=$!; BG="$BG $HN2"
reset_unit "$T/zdu1" zdu; echo $HN2 > "$T/mainpid"
rc=$(bash "$T/z/hs2-safe-deploy.sh" hs2 "$T/new" "$(sha "$T/new")" >"$T/out" 2>&1; echo $?)
check "safe-deploy OK: 12 old safe backups + the new one -> 10 left, zdu ones untouched" '[ "$rc" = 0 ] && [ "$(ls -1d "$T/st"/hs2-backup-*-pre-safe-hs2 | wc -l)" = 10 ] && [ "$(ls -1d "$T/st"/hs2-backup-*-pre-zdu-hs2 | wc -l)" = 10 ]'
exit $fail

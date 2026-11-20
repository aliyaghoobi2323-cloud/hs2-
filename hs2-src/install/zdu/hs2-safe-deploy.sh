#!/bin/bash
# hs2-safe-deploy.sh — server-side, connection-independent binary switch with automatic fallback.
# Usage (always start detached so an ssh drop cannot interrupt it):
#   systemd-run --collect --unit=hs2deploy /root/hs2-safe-deploy.sh <unit> <new-binary> <sha256-prefix>
# Steps: verify binary (exists, sha, runs `version`) → backup drop-in → switch ExecStart → restart
#        → health check (active, right exe, stays up 45 s) → record last-good; on any failure revert + restart.
# Result line: /root/hs2-safe-deploy.<unit>.out   Full log: /root/hs2-safe-deploy.log
# ZDU R3 (zdu350 S2): 30-zdu.conf (Type=notify) is only safe with a binary that sends READY=1, i.e. one whose
#   `version --features` lists zdu. Switching (or reverting) to a binary without it removes 30-zdu.conf and
#   runs daemon-reload before the restart; otherwise systemd waits for READY until TimeoutStartSec and fails.
# ZDU S8: the ExecStart drop-in is found (the last *.conf with ExecStart=/: 40-tunnel1.conf on t1,
#   50-canary.conf on t2), not hard-coded. Built from prod sha d65764d7 + proposals/safe-deploy-zdu.patch
#   (safe-deploy hunk only: prod has no hs2-start-guard.sh).
set -u
U="$1"; NEW="$2"; SHA="$3"
HERE=$(dirname "$(readlink -f "$0")")
. "$HERE/hs2-zdu-lib.sh" || { echo "ABORT missing $HERE/hs2-zdu-lib.sh"; exit 1; }
CONF=$(find_execstart_dropin "$U")
ST="${HS2_ZDU_STATE_DIR:-/root}"; ETC="${HS2_ZDU_ETC_DIR:-/etc/hs2}" # test hooks
LG="$ETC/last-good.$U.conf"
ZDU="${SYSTEMD_DIR:-/etc/systemd/system}/$U.service.d/30-zdu.conf"
ZENV="${SYSTEMD_DIR:-/etc/systemd/system}/$U.service.d/31-zdu-env.conf"
OUT="$ST/hs2-safe-deploy.$U.out"
log(){ echo "$(date -u +%FT%TZ) [$U] $*" | tee -a "$ST/hs2-safe-deploy.log"; }
fin(){ echo "$1 $(date -u +%T)" > "$OUT"; log "$1"; exit "${2:-0}"; }
# zdu_fit BIN: remove 30-zdu.conf when BIN cannot run under Type=notify (caller runs daemon-reload)
# (and 31-zdu-env.conf, HS2_ZDU=1: harmless for a pre-ZDU binary, but a rollback leaves no ZDU state)
zdu_fit(){ { [ -f "$ZDU" ] || [ -f "$ZENV" ]; } || return 0; has_zdu "$1" && return 0
  mkdir -p "$BK"; local f
  for f in "$ZDU" "$ZENV"; do [ -f "$f" ] && cp -a "$f" "$BK/$(basename "$f").prev" && rm -f "$f"; done; ZDU_RM=1
  log "R3: $1 lacks zdu -> removed 30-zdu.conf + 31-zdu-env.conf (copies in $BK)"; }
ZDU_RM=0

[ -n "$CONF" ] && [ -f "$CONF" ] || fin "ABORT no-dropin /etc/systemd/system/$U.service.d/*.conf with ExecStart" 1
CB=$(basename "$CONF")
[ -x "$NEW" ] || fin "ABORT binary-missing $NEW" 1
sha256sum "$NEW" | grep -q "^$SHA" || fin "ABORT sha-mismatch $NEW" 1
timeout 10 "$NEW" version >/dev/null 2>&1 || fin "ABORT binary-does-not-run $NEW" 1

OLD=$(grep -oE '^ExecStart=/[^ ]+' "$CONF" | head -1 | cut -d= -f2)
[ -n "$OLD" ] || fin "ABORT cannot-read-current-ExecStart" 1
[ "$OLD" = "$NEW" ] && fin "ALREADY $NEW" 0
[ -x "$OLD" ] || log "warning: current binary $OLD missing"

BK="$ST/hs2-backup-$(date +%Y%m%d-%H%M%S)-pre-safe-$U"; mkdir -p "$BK"
cp -a "$(dirname "$CONF")" "$BK/"; cp -a "$ETC" "$BK/etc-hs2" 2>/dev/null
cp -a "$CONF" "$BK/$CB.prev"
# the currently running config is, by definition, a good fallback if none recorded yet
[ -f "$LG" ] || { [ -x "$OLD" ] && mkdir -p "$ETC" && cp -a "$CONF" "$LG"; }
log "switch $OLD -> $NEW in $CB (backup $BK)"

sed -i "s#^ExecStart=$OLD #ExecStart=$NEW #" "$CONF"
grep -q "^ExecStart=$NEW " "$CONF" || { cp -a "$BK/$CB.prev" "$CONF"; fin "ABORT sed-failed (restored)" 1; }
zdu_fit "$NEW"
systemctl daemon-reload
# --no-block: under Type=notify (30-zdu.conf) a blocking restart waits for READY=1 up to TimeoutStartSec, and a
# binary that never sends it would hold this script there; the poll below bounds the wait itself (review).
systemctl restart --no-block "$U"

ok=0
# up to HS2_SD_TRIES x HS2_SD_POLL s (default 60 x 2 = 120 s > TimeoutStartSec=90 of 30-zdu.conf): a slow READY=1 is
# still "activating" (not active) here, so the loop must outlast the start timeout before it reverts.
for i in $(seq 1 "${HS2_SD_TRIES:-60}"); do
  sleep "${HS2_SD_POLL:-2}"
  P=$(systemctl show -p MainPID --value "$U")
  if systemctl is-active -q "$U" && [ "$P" != 0 ] && tr '\0' ' ' < /proc/$P/cmdline 2>/dev/null | grep -qF "$NEW "; then ok=1; break; fi
  # the start job ended without READY (TimeoutStartSec) and no auto-restart is pending: stop waiting
  [ "$(systemctl show -p ActiveState --value "$U")" = failed ] && { log "start failed (ActiveState=failed after $i polls)"; break; }
done
if [ $ok = 1 ]; then
  # must stay up (same PID) for 45 s
  P0=$(systemctl show -p MainPID --value "$U"); sleep "${HS2_SD_STABLE:-45}"
  P1=$(systemctl show -p MainPID --value "$U")
  if systemctl is-active -q "$U" && [ "$P0" = "$P1" ]; then
    cp -a "$CONF" "$LG"
    fin "OK $NEW pid=$P1" 0
  fi
  log "unstable after start (pid $P0 -> $P1)"
fi
log "health check failed — reverting to $OLD"
cp -a "$BK/$CB.prev" "$CONF"
# put 30-zdu.conf back only if this run removed it and OLD can run under it; drop it if OLD cannot
if [ "$ZDU_RM" = 1 ] && has_zdu "$OLD"; then
  for f in 30-zdu.conf 31-zdu-env.conf; do [ -f "$BK/$f.prev" ] && cp -a "$BK/$f.prev" "$(dirname "$ZDU")/$f"; done
  log "R3: restored the ZDU drop-ins for $OLD"; fi
zdu_fit "$OLD"
systemctl daemon-reload; systemctl reset-failed "$U" 2>/dev/null; systemctl restart --no-block "$U"
# non-blocking restart: a Type=notify OLD is "activating" until READY=1, so poll (bounded) for active
for i in $(seq 1 "${HS2_SD_TRIES:-60}"); do sleep "${HS2_SD_POLL:-2}"; systemctl is-active -q "$U" && break; done
systemctl is-active -q "$U" && fin "REVERTED to $OLD (active)" 2
fin "REVERTED to $OLD but NOT active — guard will retry" 3

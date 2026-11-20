#!/bin/bash
# hs2-zdu-upgrade.sh — zero-downtime binary upgrade of a running ZDU unit (zdu350 §7 S8).
# Usage (detached, so an ssh drop cannot interrupt it):
#   systemd-run --collect --unit=hs2zdu /root/hs2-zdu-upgrade.sh <unit> <new-binary> <sha256-prefix>
# Needs: the unit runs a binary with "features: zdu", HS2_ZDU=1 in its environment and 30-zdu.conf
# (Type=notify, NotifyAccess=all): see hs2-zdu-install.sh. Otherwise use hs2-safe-deploy.sh (cold restart).
# Steps: verify (sha, root-owned path, zdu feature, unit, env, drop-in, no HS2_ZDU_DRAIN < probation + 60 s) -> refuse HH:00-HH:03 UTC
#   -> refuse when the unit's HS2_* environment (drop-ins, EnvironmentFile) differs from the running process's
#      (a live handover inherits the running environment: env changes need a cold restart, hs2-safe-deploy.sh)
#   -> refuse while a previous handover's old process still drains (review 4 M1)
#   -> refuse direct (non-reverse) mode (UNIT.peers ZDU-DIRECT) -> refuse unless /run/hs2/UNIT.peers says ZDU-OK (every connected peer runs ZDU, fresh <= 30 s) ->
#   /run/hs2/UNIT.next -> SIGUSR2 to MainPID -> watch /run/hs2/UNIT.handover: PROBATION -> OK | REVERTED.
#   OK: the ExecStart drop-in points at the new binary (future cold starts), daemon-reload (no restart),
#       last-good recorded. REVERTED / refused / timeout: nothing changed; the old process serves.
# Result line: /root/hs2-zdu-upgrade.<unit>.out (OK|REVERTED|REFUSED|ABORT|TIMEOUT ...)
# Log: /root/hs2-zdu-upgrade.log. Exit: 0 OK/ALREADY, 1 ABORT, 2 REVERTED, 3 TIMEOUT, 4 REFUSED, 5 old died, 6 sed.
set -u
U="${1:?unit}"; NEW="${2:?new binary}"; SHA="${3:?sha256 prefix}"
HERE=$(dirname "$(readlink -f "$0")")
. "$HERE/hs2-zdu-lib.sh" || { echo "ABORT missing $HERE/hs2-zdu-lib.sh"; exit 1; }
RUN="${HS2_ZDU_RUN_DIR:-/run/hs2}"; NEXT="$RUN/$U.next"; HO="$RUN/$U.handover"
ZDU="${SYSTEMD_DIR:-/etc/systemd/system}/$U.service.d/30-zdu.conf"
ETC="${HS2_ZDU_ETC_DIR:-/etc/hs2}"; LG="$ETC/last-good.$U.conf"
ST="${HS2_ZDU_STATE_DIR:-/root}" # test hooks: state/run/etc dirs, /proc, lock dir
PROC="${HS2_ZDU_PROC:-/proc}"
OUT="$ST/hs2-zdu-upgrade.$U.out"
WAIT="${HS2_ZDU_UPGRADE_WAIT:-240}" # READY wait (<= 90 s) + probation (45 s) + margin
log(){ echo "$(date -u +%FT%TZ) [$U] $*" | tee -a "$ST/hs2-zdu-upgrade.log"; }
fin(){ echo "$1 $(date -u +%T)" > "$OUT"; log "$1"; exit "${2:-0}"; }
mp(){ systemctl show -p MainPID --value "$U"; }

exec 9>"${HS2_ZDU_LOCK_DIR:-/run/lock}/hs2-zdu-upgrade.$U.lock"
flock -n 9 || fin "ABORT another hs2-zdu-upgrade runs for $U" 1
CONF=$(find_execstart_dropin "$U")
[ -n "$CONF" ] || fin "ABORT no ExecStart drop-in for $U" 1
[ "${#SHA}" -ge 8 ] || fin "ABORT sha prefix shorter than 8" 1
root_owned_path "$NEW" || fin "ABORT $NEW: not a regular executable on a root-owned, non-group/other-writable path" 1
sha256sum "$NEW" | grep -q "^$SHA" || fin "ABORT sha-mismatch $NEW" 1
has_zdu "$NEW" || fin "ABORT $NEW lacks the zdu feature (use hs2-safe-deploy.sh)" 1
{ [ -f "$ZDU" ] && grep -q '^Type=notify' "$ZDU" && grep -q '^NotifyAccess=all' "$ZDU"; } ||
  fin "ABORT $ZDU missing or not Type=notify + NotifyAccess=all (hs2-zdu-install.sh first)" 1
systemctl is-active -q "$U" || fin "ABORT $U not active" 1
PID=$(mp)
{ [ -n "$PID" ] && [ "$PID" != 0 ]; } || fin "ABORT no MainPID" 1
OLDEXE=$(readlink "$PROC/$PID/exe" | sed 's/ (deleted)$//')
tr '\0' '\n' < "$PROC/$PID/environ" | grep -qx 'HS2_ZDU=1' || fin "ABORT pid $PID runs without HS2_ZDU=1 (hs2-zdu-install.sh first)" 1
has_zdu "$OLDEXE" || fin "ABORT running binary $OLDEXE lacks zdu" 1
# HS2_ZDU_DRAIN counts from SIGUSR2 and includes the 45 s probation (drill b1: 40 s cut every call at OK).
# The running process's own environment is what this handover uses (the new process inherits it).
DR=$(env_of "$PROC/$PID/environ" HS2_ZDU_DRAIN); PR=$(env_of "$PROC/$PID/environ" HS2_ZDU_PROBATION)
drain_override_ok "$DR" "$PR" || fin "ABORT pid $PID has HS2_ZDU_DRAIN=$DR: below probation ${PR:-45s} + 60 s (or unparseable); unset it (HH:03 rule) or raise it" 1
DD=$(dropin_env "$(dirname "$CONF")" HS2_ZDU_DRAIN); DP=$(dropin_env "$(dirname "$CONF")" HS2_ZDU_PROBATION)
drain_override_ok "$DD" "$DP" || fin "ABORT a drop-in of $U sets HS2_ZDU_DRAIN=$DD: below probation ${DP:-45s} + 60 s (or unparseable)" 1
OLD=$(grep -oE '^ExecStart=/[^ ]+' "$CONF" | head -1 | cut -d= -f2)
[ "$OLD" = "$NEW" ] && fin "ALREADY $NEW in $(basename "$CONF")" 0
# HH:00-HH:03:59 UTC (the hourly xray restart storm): refused here, and by the process too
M=$(date -u +%-M)
[ -n "${HS2_ZDU_UPGRADE_FAKE_MIN:-}" ] && M=$HS2_ZDU_UPGRADE_FAKE_MIN # lab only
# The restart minute follows the Kharej xray-loop's phase (sleep 3600, seen at HH:01 UTC), not a time zone:
# HS2_ZDU_STORM_MIN in the running process's environment moves the window [M-1, M+3) (default 1).
SM=$(storm_min "$(env_of "$PROC/$PID/environ" HS2_ZDU_STORM_MIN)")
REL=$(( (M - (SM + 59) % 60 + 60) % 60 )) # minutes since the window opened
[ "$REL" -le 3 ] && fin "REFUSED window HH:$(printf %02d $(( (SM+59)%60 )))-HH:$(printf %02d $(( (SM+2)%60 ))) UTC (minute $M)" 4
[ "$REL" -ge 58 ] && fin "REFUSED window opens in <= 2 min (minute $M): READY + probation would run into the restart storm" 4
# Every connected peer must run ZDU (capExtZDU + instance): without it the far side has no generation pin
# (review F2). The running process rewrites $RUN/$U.peers every 5 s; the process re-checks at SIGUSR2.
PF="$RUN/$U.peers"; P=$(cat "$PF" 2>/dev/null)
[ -n "$P" ] || fin "REFUSED no $PF: the running process does not report its peer check (older ZDU build: hs2-safe-deploy.sh)" 4
PA=$(( $(date +%s) - $(stat -c %Y "$PF") ))
[ "$PA" -le 30 ] || fin "REFUSED $PF is ${PA}s old (stale)" 4
case "$P" in ZDU-DIRECT*) fin "REFUSED direct (non-reverse) mode: live handover is supported in reverse mode only (only Iran dials in direct mode; the new Kharej would get no links of its own). Use a cold restart (hs2-safe-deploy.sh)" 4;; esac
case "$P" in ZDU-OK*) ;; *) fin "REFUSED peers: $P (every connected peer must run ZDU; upgrade the other side first)" 4;; esac
# Review 4 M1: no new handover while the previous one's old process still drains (3 processes / 3
# generations were never designed). The running MainPID is then a hand-off child whose parent (the old
# process) is alive: its PPid is not 1. UNIT.handover says OK/PROBATION until the old logs "drain end"
# (it writes DRAIN-END / DRAIN-ABORT). The process refuses too (zduParentDraining).
PPID_MP=$(sed -n 's/^PPid:[[:space:]]*//p' "$PROC/$PID/status" 2>/dev/null)
HOS=$(cat "$HO" 2>/dev/null)
case "$HOS" in OK*|PROBATION*)
  { [ -n "$PPID_MP" ] && [ "$PPID_MP" != 1 ] && [ "$PPID_MP" != 0 ]; } &&
    fin "REFUSED drain running: the previous handover's old process (pid $PPID_MP) still drains ($HOS); wait for its \"drain end\" (UNIT.handover DRAIN-END) and retry" 4;;
esac
# The new process inherits the RUNNING process's environment: an Environment=/EnvironmentFile= change in a
# drop-in only applies at a cold restart. Refuse when they differ, so a changed key never silently waits for
# the next unplanned restart. daemon-reload first (no restart) so systemd shows the drop-ins on disk.
[ "$(systemctl show -p NeedDaemonReload --value "$U" 2>/dev/null)" = yes ] && { systemctl daemon-reload; log "daemon-reload (unit files changed on disk)"; }
DRIFT=$(env_drift "$U" "$PROC/$PID/environ")
[ -z "$DRIFT" ] || fin "REFUSED env changed: $(echo $DRIFT) (+ = set by the unit now, - = only in the running process). A live handover keeps the running environment; apply it with a cold restart (hs2-safe-deploy.sh) or undo the drop-in change" 4

[ -d "$RUN" ] || mkdir -m 0700 -p "$RUN"  # b396: rel69q /run/hs2 is 0700 root (sec370); never widen an existing dir
T=$(mktemp "$RUN/.next.XXXXXX"); printf '%s\n' "$NEW" > "$T"; chmod 0600 "$T"; chown 0:0 "$T"; mv -f "$T" "$NEXT"
rm -f "$HO"
log "upgrade $OLDEXE (pid $PID) -> $NEW: SIGUSR2"
T0=$(date +%s); SINCE="@$((T0-1))"
kill -USR2 "$PID" || fin "ABORT kill -USR2 $PID failed" 1
state=""; seen=""
while [ $(( $(date +%s) - T0 )) -lt "$WAIT" ]; do
  s=$(cat "$HO" 2>/dev/null)
  if [ -n "$s" ] && [ "$s" != "$seen" ]; then seen="$s"; log "handover: $s (t+$(( $(date +%s)-T0 ))s, MainPID $(mp))"; fi
  case "$s" in OK*) state=OK; break;; REVERTED*) state=REVERTED; break;; esac
  if [ -z "$s" ]; then
    r=$(journalctl -u "$U" --since "$SINCE" --no-pager -o cat 2>/dev/null | grep 'zdu: handover refused' | tail -1)
    [ -n "$r" ] && { rm -f "$NEXT"; fin "REFUSED by pid $PID: $r" 4; }
    kill -0 "$PID" 2>/dev/null || fin "ABORT old pid $PID died before any handover state; unit $(systemctl is-active "$U")" 5
  fi
  sleep 0.5
done
rm -f "$NEXT"
[ "$state" = REVERTED ] && fin "REVERTED ($seen); old pid $PID serving: MainPID $(mp) $(systemctl is-active "$U"); drop-in unchanged ExecStart=$OLD" 2
[ "$state" = OK ] || fin "TIMEOUT no OK/REVERTED in ${WAIT}s (last: ${seen:-none}); drop-in unchanged" 3
NP=$(echo "$seen" | awk '{print $2}')
[ "$(mp)" = "$NP" ] || log "warning: MainPID $(mp) != handover pid $NP"
BK="$ST/hs2-backup-$(date +%Y%m%d-%H%M%S)-pre-zdu-$U"; mkdir -p "$BK"
cp -a "$(dirname "$CONF")" "$BK/"
sed -i "s#^ExecStart=$OLD #ExecStart=$NEW #" "$CONF"
grep -q "^ExecStart=$NEW " "$CONF" || { cp -a "$BK/$U.service.d/$(basename "$CONF")" "$CONF"; fin "OK-LIVE pid $NP but the ExecStart sed failed (drop-in left at $OLD: a cold start runs the old binary)" 6; }
systemctl daemon-reload
mkdir -p "$ETC"; cp -a "$CONF" "$LG"
fin "OK $NEW pid=$NP ($(basename "$CONF") ExecStart switched, last-good recorded, backup $BK)" 0

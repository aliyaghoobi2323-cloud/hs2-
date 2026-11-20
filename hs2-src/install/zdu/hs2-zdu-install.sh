#!/bin/bash
# hs2-zdu-install.sh — turn ZDU on for a unit (zdu350 §7 S8, correction R3): ONE cold restart installs
# 30-zdu.conf (Type=notify, NotifyAccess=all, FileDescriptorStoreMax=32) and 31-zdu-env.conf (HS2_ZDU=1)
# together with a ZDU binary. Never install 30-zdu.conf alone under a running pre-ZDU binary: the next
# restart would wait for READY=1 until TimeoutStartSec and fail.
# Usage (detached): systemd-run --collect --unit=hs2zduinst /root/hs2-zdu-install.sh <unit> <zdu-binary> <sha256-prefix>
# Removal:          hs2-zdu-install.sh --remove <unit>   (both drop-ins + daemon-reload; plain at the next restart)
# It writes the two drop-ins, then runs hs2-safe-deploy.sh (switch ExecStart, restart, 45 s health check,
# revert on failure). On a failed deploy both drop-ins go again.
set -u
HERE=$(dirname "$(readlink -f "$0")")
. "$HERE/hs2-zdu-lib.sh" || { echo "ABORT missing $HERE/hs2-zdu-lib.sh"; exit 1; }
if [ "${1:-}" = --remove ]; then
  U="${2:?unit}"; D="${SYSTEMD_DIR:-/etc/systemd/system}/$U.service.d"
  rm -f "$D/30-zdu.conf" "$D/31-zdu-env.conf"; systemctl daemon-reload
  echo "removed 30-zdu.conf 31-zdu-env.conf for $U (takes effect at the next restart)"; exit 0
fi
U="${1:?unit}"; NEW="${2:?zdu binary}"; SHA="${3:?sha256 prefix}"
D="${SYSTEMD_DIR:-/etc/systemd/system}/$U.service.d"
log(){ echo "$(date -u +%FT%TZ) [$U] zdu-install: $*" | tee -a "${HS2_ZDU_STATE_DIR:-/root}/hs2-safe-deploy.log"; }
root_owned_path "$NEW" || { log "ABORT $NEW not on a root-owned path"; exit 1; }
sha256sum "$NEW" | grep -q "^$SHA" || { log "ABORT sha-mismatch $NEW"; exit 1; }
has_zdu "$NEW" || { log "ABORT $NEW lacks zdu"; exit 1; }
CONF=$(find_execstart_dropin "$U")
[ -n "$CONF" ] || { log "ABORT no ExecStart drop-in"; exit 1; }
DD=$(dropin_env "$D" HS2_ZDU_DRAIN); DP=$(dropin_env "$D" HS2_ZDU_PROBATION)
drain_override_ok "$DD" "$DP" || { log "ABORT a drop-in sets HS2_ZDU_DRAIN=$DD: below probation ${DP:-45s} + 60 s (or unparseable)"; exit 1; }
# same path as the running ExecStart: safe-deploy would say ALREADY without a restart, so the drop-ins
# would wait on disk for an unplanned restart. Install the ZDU binary under a new path instead.
grep -q "^ExecStart=$NEW " "$CONF" && { log "ABORT $NEW is already the ExecStart in $(basename "$CONF"): use a new path"; exit 1; }
M=$(date -u +%-M)
[ -n "${HS2_ZDU_UPGRADE_FAKE_MIN:-}" ] && M=$HS2_ZDU_UPGRADE_FAKE_MIN # lab only
[ "$M" -le 3 ] && { log "REFUSED window HH:00-HH:03 UTC"; exit 4; }
# TimeoutStartSec explicit (= the usual default): a cold start that never sends READY=1 fails after 90 s even
# when the host's DefaultTimeoutStartSec is raised; hs2-safe-deploy.sh polls 120 s, longer than this.
printf '[Service]\nType=notify\nNotifyAccess=all\nFileDescriptorStoreMax=32\nTimeoutStartSec=90\n' > "$D/30-zdu.conf"
printf '[Service]\nEnvironment=HS2_ZDU=1\n' > "$D/31-zdu-env.conf"
log "installed 30-zdu.conf + 31-zdu-env.conf; cold switch through hs2-safe-deploy.sh"
"$HERE/hs2-safe-deploy.sh" "$U" "$NEW" "$SHA"; rc=$?
if [ $rc != 0 ]; then
  rm -f "$D/30-zdu.conf" "$D/31-zdu-env.conf"; systemctl daemon-reload
  log "safe-deploy rc=$rc: ZDU drop-ins removed again (they apply at the next restart)"
fi
exit $rc

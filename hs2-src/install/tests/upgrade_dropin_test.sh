#!/bin/bash
# rev53c: upgrade with a drop-in that overrides ExecStart= to another binary
# (classic r31 installs: `systemctl edit hs2` → ExecStart=/usr/local/bin/hs2-r31
# run -c …). Without dropin_exec_sync the upgrade replaced $BIN, restarted the
# unit and reported "upgraded" while the old binary kept running.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
sed -n '/^dropin_exec_sync(){/,/^}/p;/^running_exe_ok(){/,/^}/p' "$INST" > "$T/fns.sh"
for f in dropin_exec_sync running_exe_ok; do grep -q "^$f(){" "$T/fns.sh" || { echo "FAIL $f not found in install.sh"; exit 1; }; done
mkdir -p "$T/bin" "$T/units/hs2.service.d" "$T/units/hs2-b.service.d" "$T/units/hs2-c.service.d"
printf '#!/bin/sh\necho "$@" >> %s/sd\n' "$T" > "$T/bin/systemctl"; chmod +x "$T/bin/systemctl"
cat > "$T/units/hs2.service.d/override.conf" <<'C'
[Service]
ExecStart=
ExecStart=/usr/local/bin/hs2-r31 run -c /etc/hs2/config.json
Environment=HS2_MCONN=lite HS2_L3_DUP=1
C
cat > "$T/units/hs2-b.service.d/10-env.conf" <<'C'
[Service]
Environment=HS2_LB_AUTO2=1
C
cat > "$T/units/hs2-c.service.d/wrap.conf" <<'C'
[Service]
ExecStart=
ExecStart=/usr/bin/taskset -c 1 /usr/local/bin/hs2 run -c /etc/hs2/c.json
C
run(){ PATH="$T/bin:$PATH" bash -c "ok(){ echo OK: \$* >&2; }; warn(){ echo WARN: \$* >&2; }; err(){ echo ERR: \$* >&2; }
BIN=/usr/local/bin/hs2; UNIT_DIR='$T/units'; tm_prop(){ echo \"\${PID_OVERRIDE:-0}\"; }
source '$T/fns.sh'; $1" 2>&1; }
out=$(run 'dropin_exec_sync hs2')
check "an ExecStart= to another binary now runs \$BIN with the same arguments" 'grep -qx "ExecStart=/usr/local/bin/hs2 run -c /etc/hs2/config.json" "$T/units/hs2.service.d/override.conf"'
check "the reset line and the Environment= keys are kept" 'grep -qx "ExecStart=" "$T/units/hs2.service.d/override.conf" && grep -q "HS2_L3_DUP=1" "$T/units/hs2.service.d/override.conf"'
check "the old drop-in is kept under a name systemd ignores" 'grep -q hs2-r31 "$T/units/hs2.service.d/override.conf.pre-upgrade"'
check "the operator is told" 'echo "$out" | grep -q "ran another binary (/usr/local/bin/hs2-r31)"'
check "systemd is reloaded" 'grep -qx daemon-reload "$T/sd"'
: > "$T/sd"
out=$(run 'dropin_exec_sync hs2-b')
check "an Environment-only drop-in is not touched (no reload, no copy)" '[ ! -s "$T/sd" ] && [ ! -e "$T/units/hs2-b.service.d/10-env.conf.pre-upgrade" ]'
out=$(run 'dropin_exec_sync hs2-c')
check "a wrapper ExecStart that already runs \$BIN (taskset …) is neither edited nor warned" 'grep -q "taskset -c 1" "$T/units/hs2-c.service.d/wrap.conf" && [ -z "$out" ]'
sed -i "s#/usr/local/bin/hs2 run#/opt/old/hs2 run#" "$T/units/hs2-c.service.d/wrap.conf"
out=$(run 'dropin_exec_sync hs2-c')
check "a wrapper around another binary is reported, never edited" 'grep -q "taskset -c 1 /opt/old/hs2" "$T/units/hs2-c.service.d/wrap.conf" && echo "$out" | grep -q "left as is"'
out=$(run 'dropin_exec_sync hs2')
check "idempotent: a second run changes nothing" 'echo "$out" | grep -vq "ran another" ; ! echo "$out" | grep -q "ran another"'
out=$(run 'dropin_exec_sync hs2-none')
check "a unit with no drop-in dir is fine" '[ -z "$out" ]'
# running_exe_ok: our own shell's exe is not $BIN → error; no PID → ok
out=$(PID_OVERRIDE=$$ run 'running_exe_ok hs2 && echo RC0 || echo RC1')
check "running_exe_ok: a process that is not \$BIN fails with the path" 'echo "$out" | grep -q RC1 && echo "$out" | grep -q "not the upgraded"'
out=$(run 'running_exe_ok hs2 && echo RC0 || echo RC1')
check "running_exe_ok: no PID → no false error" 'echo "$out" | grep -qx RC0'
body=$(sed -n '/^upgrade(){/,/^}/p' "$INST")
check "upgrade calls dropin_exec_sync and checks the running binary" 'echo "$body" | grep -q "dropin_exec_sync \"\$u\"" && echo "$body" | grep -q "running_exe_ok \"\$u\""'
bk=$(sed -n '/^backup(){/,/^}/p' "$INST")
check "backup keeps each tunnel's drop-ins (its HS2_ keys)" 'echo "$bk" | grep -q "items+=(\"\${UNIT_DIR#/}/\$u.service.d\")"'
# tm_units: the helper units (alert/stats/lb-park timers' services) are not tunnels
sed -n '/^tm_units(){/,/^}/p' "$INST" > "$T/tmu.sh"
mkdir -p "$T/u2"
printf '[Service]\nType=simple\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/config.json\n' > "$T/u2/hs2.service"
printf '[Service]\nType=simple\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/de1.json\n' > "$T/u2/hs2-de1.service"
printf '[Service]\nType=oneshot\nNice=10\nExecStart=/usr/local/bin/hs2 alert -c /etc/hs2/config.json -u hs2\n' > "$T/u2/hs2-alert-hs2.service"
printf '[Unit]\nDescription=x\n[Service]\nType=oneshot\nNice=19\nExecStart=/usr/local/bin/hs2 stats sample\n' > "$T/u2/hs2-stats.service"
ln -s /dev/null "$T/u2/hs2-masked.service"
got=$(bash -c "UNIT_DIR='$T/u2'; source '$T/tmu.sh'; tm_units" | tr '\n' ' ')
check "tm_units: tunnels and a masked unit, never hs2-alert-*/hs2-stats (got: $got)" '[ "$got" = "hs2 hs2-de1 hs2-masked " ]'
# heal_alert_units: an alert unit an older upgrade turned into a 2nd tunnel daemon
sed -n '/^heal_alert_units(){/,/^}/p;/^tm_cfg(){/p' "$INST" > "$T/heal.sh"
mkdir -p "$T/u3"
printf '[Service]\nType=simple\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/config.json\n' > "$T/u3/hs2.service"
cp "$T/u3/hs2.service" "$T/u3/hs2-alert-hs2.service"
printf '[Service]\nType=simple\nExecStart=/usr/local/bin/hs2 run -c /etc/hs2/other.json\n' > "$T/u3/hs2-alert-x.service"
: > "$T/sd"
out=$(PATH="$T/bin:$PATH" bash -c "warn(){ echo WARN: \$* >&2; }; alert_enable(){ echo \"alert_enable \$1 \$2\"; }
UNIT_DIR='$T/u3'; source '$T/heal.sh'; heal_alert_units" 2>&1)
check "heal: hs2-alert-hs2 running hs2's config is stopped and re-made as the alert check" 'echo "$out" | grep -qx "alert_enable hs2 /etc/hs2/config.json" && grep -q "disable --now hs2-alert-hs2.service" "$T/sd"'
check "heal: a real tunnel named alert-x (own config, no unit x) is left alone" '! echo "$out" | grep -q "alert_enable x"'
check "upgrade heals before listing tunnels" 'echo "$body" | grep -q "^  heal_alert_units"'
exit $fail

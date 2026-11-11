#!/bin/bash
# The new main menu (menu redesign M2: `hs2-menu --new`), driven through a
# real pty under the installer's `set -euo pipefail`: navigation to every
# item, the settings screen and its persistence in menu.conf, the admin lock
# around edit screens (and a lock held by another admin), the dashboard call,
# the maintenance history/rollback items, which menu a bare hs2-menu opens,
# and the ASCII/English fallback when the locale is not UTF-8.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
check "dispatcher knows --new and --classic" 'grep -q "^  --new)     NM_FORCE=new" "$INST" && grep -q "^  --classic) NM_FORCE=classic" "$INST"'
check "a bare hs2-menu goes through menu_entry" '[ "$(tail -n 1 "$INST")" = menu_entry ]'

# fake hs2: logs every call; LOCKED=1 = another admin holds the lock
cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
case "$1 ${2:-}" in
  "dashboard --header") echo "testhost · 1 tunnels · 7 users · 1.0 Mbit · health OK · 0 warnings"; exit 0 ;;
  "dashboard "*) echo "DASHBOARD-FRAME"; exit 0 ;;
  "config lock") if [ "${LOCKED:-0}" = 1 ]; then echo "ERROR: another admin is editing: bob (pid 4242, pts/3) since 10:00 — wait, or use --force"; exit 1; fi; echo locked; exit 0 ;;
  "config history") echo "v1 2026-11-10 setup"; echo "v2 2026-11-11 edit"; exit 0 ;;
esac
exit 0
B
chmod +x "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T LOCKED
MENU_CONF="$T/menu.conf"; UNIT_DIR="$T/units"; mkdir -p "$UNIT_DIR"
hr(){ :; }
tm_units(){ echo hs2-x; }; tm_cfg(){ echo "$T/x.json"; }; unit_desc(){ echo "test tunnel"; }
systemctl(){ echo "systemctl $*" >> "$T/calls"; return 1; }
server_doctor(){ echo "DOCTOR lang=$HS2_DOCTOR_LANG" >> "$T/calls"; }
tm_alerts(){ echo "ALERTS $1 lock=${HS2_LOCK_PID:-}" >> "$T/calls"; }
status(){ echo STATUS >> "$T/calls"; }
main_menu(){ echo "CLASSIC" >> "$T/calls"; }
if [ -n "${ENTRY:-}" ]; then menu_entry; else nm_main; fi
echo "MENU_DONE"
INNER
run(){ rm -f "$T/calls"; printf "$1" | script -qec "T='$T' LOCKED=${LOCKED:-0} LANG=${TLANG:-en_US.UTF-8} LC_ALL= LC_CTYPE= ENTRY='${ENTRY:-}' HS2_MENU='${HMENU:-}' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }

rm -f "$T/menu.conf"
out=$(run '0\n')
check "default: Persian on a UTF-8 terminal" 'echo "$out" | grep -q "داشبورد زنده" && echo "$out" | grep -q "پزشک سرور"'
check "header from hs2 dashboard --header" 'echo "$out" | grep -q "testhost · 1 tunnels · 7 users" && grep -q "^dashboard --header --lang fa" "$T/calls"'
check "exit leaves the menu cleanly" '! echo "$out" | grep -q "stopped unexpectedly"'

out=$(run '9\n1\n3\n2\n0\n0\n')
check "settings saved: lang/colour/mode" 'grep -qx "lang=en" "$T/menu.conf" && grep -qx "color=off" "$T/menu.conf" && grep -qx "mode=advanced" "$T/menu.conf"'
out=$(run '0\n')
check "settings persist: English + advanced descriptions on the next start" 'echo "$out" | grep -q "1) Live dashboard" && echo "$out" | grep -q "hs2 doctor --deep: NIC"'
check "colour off: no escape codes in the menu" '! echo "$out" | grep -q $'"'"'\033\[1;3'"'"''
out=$(run '9\n1\n0\n0\n')
check "language toggles back to fa" 'grep -qx "lang=fa" "$T/menu.conf" && [ "$(grep -c "^lang=" "$T/menu.conf")" = 1 ]'
printf 'lang=en\nmode=simple\ncolor=off\n' > "$T/menu.conf"

out=$(run '1\n0\n')
check "1) dashboard: hs2 dashboard in the chosen language, colour off" 'grep -q "^dashboard --lang en --color off$" "$T/calls" && echo "$out" | grep -q DASHBOARD-FRAME'
out=$(run '7\n\n0\n')
check "7) server doctor in the menu language" 'grep -q "^DOCTOR lang=en" "$T/calls"'
out=$(run '6\n0\n')
check "6) alerts open under the admin lock, then unlock" 'grep -q "^config lock --pid" "$T/calls" && grep -q "^ALERTS hs2-x lock=[0-9]" "$T/calls" && grep -q "^config unlock --pid" "$T/calls"'
out=$(LOCKED=1 run '6\nn\n0\n')
check "locked by another admin: told who, screen not opened" 'echo "$out" | grep -q "another admin is editing: bob" && ! grep -q "^ALERTS" "$T/calls"'
out=$(run '8\n4\n\n0\n0\n')
check "8) maintenance → change history" 'grep -q "^config history -u hs2-x --lang en" "$T/calls" && echo "$out" | grep -q "v2 2026-11-11 edit"'
out=$(run '8\n5\ny\n\n0\n0\n')
check "8) maintenance → rollback last change (asked, locked)" 'grep -q "^rollback -u hs2-x --lang en" "$T/calls" && grep -q "^config lock" "$T/calls"'
out=$(run '8\n5\nn\n\n0\n0\n')
check "rollback declined: nothing rolled back" '! grep -q "^rollback" "$T/calls"'
out=$(run '2\n0\n')
check "2) tunnels: the M3 Tunnels screen (hs2 tunnel list)" 'grep -q "^tunnel list --lang en" "$T/calls"'
out=$(run '5\n0\n0\n')
check "5) speed: the M4 Speed & performance screen (hs2 perf show)" 'grep -q "^perf show -u hs2-x" "$T/calls" && ! echo "$out" | grep -q "arrives in M"'
out=$(run '4
0
0
')
check "4) security: still marked as a preview of M5" 'echo "$out" | grep -q "arrives in M5"'
out=$(run 'x\n0\n')
check "invalid choice survives (menu drawn again)" 'echo "$out" | grep -q "Invalid choice" && [ "$(echo "$out" | grep -c "1) Live dashboard")" -ge 2 ]'
out=$(run '9\n4\n0\n0\n')
check "settings: default menu → new" 'grep -qx "default_menu=new" "$T/menu.conf"'

out=$(ENTRY=1 run '0\n')
check "hs2-menu: default_menu=new opens the new menu" '! grep -q CLASSIC "$T/calls" && echo "$out" | grep -q "Live dashboard"'
out=$(ENTRY=1 HMENU=classic run '0\n')
check "HS2_MENU=classic overrides menu.conf" 'grep -q CLASSIC "$T/calls"'
rm -f "$T/menu.conf"
out=$(ENTRY=1 run '0\n')
check "hs2-menu: classic stays the default" 'grep -q CLASSIC "$T/calls"'
out=$(TLANG=C run '0\n')
check "non-UTF-8 locale: English" 'echo "$out" | grep -q "1) Live dashboard" && ! echo "$out" | grep -q "داشبورد"'
exit "$fail"

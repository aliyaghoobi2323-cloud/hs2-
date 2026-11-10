#!/bin/bash
# Menu M3, screen 2 "Tunnels" (`hs2-menu --new` → 2), driven through a real
# pty under the installer's `set -euo pipefail` with a fake hs2 that logs its
# calls: the list, the wizard (create / advanced), join with a code, the
# per-tunnel actions (start/stop/restart/edit/logs/code/clone/renew/delete),
# the admin lock around every changing action, an aborted wizard, and the
# fallback to the classic screens when the binary has no `hs2 tunnel`.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"

cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
[ "${OLDBIN:-0}" = 1 ] && [ "$1" = tunnel ] && { echo "unknown command"; exit 2; }
case "$1 ${2:-}" in
  "dashboard --header") echo "testhost · 2 tunnels"; exit 0 ;;
  "tunnel list")
    if [ "${3:-}" = --names ]; then printf 'hs2-a\nhs2-b\n'; exit 0; fi
    echo "  #  tunnel  state"; echo "  1  hs2-a   active"; echo "  2  hs2-b   inactive"; exit 0 ;;
  "tunnel create")
    read -r -p "1/6 Role: " a </dev/tty || { echo "Cancelled — nothing was changed"; exit 3; }
    [ "$a" = q ] && { echo "Cancelled — nothing was changed on this server."; exit 3; }
    echo "WIZARD-CREATED role=$a"; echo "hs2 tunnel join 'hs2://v2.TESTCODE'"; exit 0 ;;
  "tunnel delete") echo "DELETED"; exit 0 ;;
  "config lock") echo locked; exit 0 ;;
esac
exit 0
B
chmod +x "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T OLDBIN
MENU_CONF="$T/menu.conf"; UNIT_DIR="$T/units"; mkdir -p "$UNIT_DIR"
printf 'lang=en\nmode=%s\ncolor=off\n' "${MODE:-simple}" > "$MENU_CONF"
hr(){ :; }
tm_units(){ echo hs2-a; }; tm_cfg(){ echo "$T/a.json"; }; unit_desc(){ echo "test"; }
systemctl(){ return 1; }
tunnel_manager(){ echo "CLASSIC-TM" >> "$T/calls"; }
nm_main
echo "MENU_DONE"
INNER
run(){ rm -f "$T/calls"; printf "$1" | script -qec "T='$T' OLDBIN=${OLDBIN:-0} MODE=${MODE:-simple} LANG=en_US.UTF-8 LC_ALL= LC_CTYPE= bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }

out=$(run '2\n0\n0\n')
check "screen lists the tunnels via hs2 tunnel list" 'echo "$out" | grep -q "hs2-b   inactive" && grep -q "^tunnel list --lang en" "$T/calls"'
check "simple mode hides the advanced wizard" '! echo "$out" | grep -q "a) New tunnel — advanced"'
check "menu exits cleanly" '! echo "$out" | grep -q "stopped unexpectedly"'

out=$(run '2\nn\nkharej\n\n0\n0\n')
check "wizard happy path: create under the admin lock, code shown" 'grep -q "^tunnel create --lang en" "$T/calls" && echo "$out" | grep -q "WIZARD-CREATED role=kharej" && echo "$out" | grep -q "hs2://v2.TESTCODE" && grep -q "^config lock" "$T/calls" && grep -q "^config unlock" "$T/calls"'
out=$(run '2\nn\nq\n\n0\n0\n')
check "wizard aborted: says nothing changed, menu continues, lock released" 'echo "$out" | grep -q "nothing was changed" && [ "$(grep -c "^tunnel list --lang" "$T/calls")" -ge 2 ] && grep -q "^config unlock" "$T/calls"'
out=$(MODE=advanced run '2\na\nkharej\n\n0\n0\n')
check "advanced mode: wizard with --advanced" 'grep -q "^tunnel create --advanced --lang en" "$T/calls"'
out=$(run '2\nj\nhs2://v2.ABC\n\n0\n0\n')
check "join with a pasted code" 'grep -q "^tunnel join hs2://v2.ABC --lang en" "$T/calls"'
out=$(run '2\nj\n\n0\n0\n')
check "join with an empty code does nothing" '! grep -q "^tunnel join" "$T/calls"'

out=$(run '2\n2\n1\n\n2\n\n3\n\n5\n\n6\n\n7\n\n8\n\n0\n0\n0\n')
check "actions on #2: start/stop/restart" 'grep -q "^tunnel start -u hs2-b --lang en" "$T/calls" && grep -q "^tunnel stop -u hs2-b --lang en" "$T/calls" && grep -q "^tunnel restart -u hs2-b --lang en" "$T/calls"'
check "actions: logs/code/clone/renew" 'grep -q "^tunnel logs -u hs2-b -n 60" "$T/calls" && grep -q "^tunnel code -u hs2-b" "$T/calls" && grep -q "^tunnel clone -u hs2-b --to-code" "$T/calls" && grep -q "^tunnel renew -u hs2-b --pick-ip" "$T/calls"'
out=$(run '2\n1\n4\nmtu=1360\n\n0\n0\n0\n')
check "edit: section view then --set under the lock" 'grep -q "^tunnel edit -u hs2-a --lang en" "$T/calls" && grep -q "^tunnel edit -u hs2-a --set mtu=1360 --lang en" "$T/calls"'
out=$(run '2\n1\n4\n\n0\n0\n0\n')
check "edit with no change: nothing applied" '! grep -q -- "--set" "$T/calls"'
out=$(run '2\n1\n9\n\n0\n0\n0\n')
check "delete goes through hs2 tunnel delete (its own two-step confirm)" 'grep -q "^tunnel delete -u hs2-a --lang en" "$T/calls"'
out=$(run '2\n7\n0\n0\n')
check "unknown tunnel number is refused" 'echo "$out" | grep -q "No tunnel #7"'

out=$(OLDBIN=1 run '2\n1\n0\n')
check "old binary: falls back to the classic tunnel screens" 'echo "$out" | grep -q "arrives in M3" && grep -q "^CLASSIC-TM" "$T/calls"'

[ $fail = 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }

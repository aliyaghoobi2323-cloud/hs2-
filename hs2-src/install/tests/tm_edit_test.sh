#!/bin/bash
# tm_edit (tunnel manager -> "Edit config"), driven through the REAL
# tm_tunnel_menu with the installer's REAL shell options (set -euo pipefail)
# and a genuine tty (via script(1)):
#   - Edit -> close the editor -> Back must return to the caller. A RETURN trap
#     in tm_edit once leaked past it and fired when the MENU returned, where
#     $tmp is unbound: "tmp: unbound variable", installer dead after every edit.
#   - the key-bearing edit temp is gone after every path, including SIGTERM
#     while the editor is open and a set -e abort mid-apply (on_exit cleans it).
#   - install.sh contains no `trap … RETURN` at all.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

if grep -nE "^[^#]*trap .*RETURN" "$INST" >/dev/null; then
  echo "FAIL install.sh must not use a RETURN trap (it leaks past the function)"; fail=1
else
  echo "PASS install.sh has no RETURN trap"
fi

if ! command -v script >/dev/null 2>&1; then
  echo "SKIP pty tests (script(1) not installed)"; exit "$fail"
fi

# Everything before the CLI dispatch: all functions + the real `set -euo pipefail`.
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
printf '#!/bin/bash\nprintf "\\n" >> "$1"\n' > "$T/ed_change.sh"
printf '#!/bin/bash\nkill -TERM "$PPID"\n' > "$T/ed_kill.sh"
chmod +x "$T/ed_change.sh" "$T/ed_kill.sh"

cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh" >/dev/null 2>&1
CFG_DIR="$T/cfg"; mkdir -p "$CFG_DIR"; CFGF="$CFG_DIR/hs2-x.json"; echo '{"mode":"listen"}' > "$CFGF"
tm_cfg(){ echo "$CFGF"; }; tm_details(){ :; }; tm_autostart(){ return 0; }
say(){ :; }; info(){ :; }; ok(){ :; }; warn(){ :; }; err(){ :; }; hr(){ :; }
systemctl(){ :; }; tm_healthy(){ return 0; }; tm_log_since(){ :; }; tm_validate(){ return 0; }
tm_bilateral_changed(){ :; }
case "$SCEN" in
  nochange) tm_editor(){ echo true; } ;;
  change)   tm_editor(){ echo "$T/ed_change.sh"; } ;;
  kill)     tm_editor(){ echo "$T/ed_kill.sh"; } ;;
  seterr)   tm_editor(){ echo "$T/ed_change.sh"; }; tm_bilateral_changed(){ return 3; } ;;
esac
outer(){ tm_tunnel_menu hs2-x; echo "BACK_IN_CALLER"; }
outer
INNER

run(){ rm -rf "$T/cfg"; printf "$2" | script -qec "T='$T' SCEN=$1 bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
temps(){ find "$T/cfg" -name '.hs2-edit.*' 2>/dev/null | wc -l; }

out=$(run nochange '4\n\n0\n')
check "Edit, no change, Back: menu returns (no unbound-variable crash)" 'echo "$out" | grep -q BACK_IN_CALLER && ! echo "$out" | grep -q unbound'
check "Edit, no change: no edit temp left" '[ "$(temps)" = 0 ]'
out=$(run change '4\n\n0\n')
check "Edit, saved change, Back: menu returns" 'echo "$out" | grep -q BACK_IN_CALLER && ! echo "$out" | grep -q unbound'
check "Edit, saved change: config written, no edit temp left" 'grep -q listen "$T/cfg/hs2-x.json" && [ "$(temps)" = 0 ]'
out=$(run change '4\n\n4\n\n0\n')
check "Edit twice, then Back: menu returns" 'echo "$out" | grep -q BACK_IN_CALLER && ! echo "$out" | grep -q unbound'
run kill '4\n\n' >/dev/null
check "SIGTERM while the editor is open: edit temp removed, config intact" '[ "$(temps)" = 0 ] && grep -q listen "$T/cfg/hs2-x.json"'
run seterr '4\n\n' >/dev/null
check "set -e abort mid-apply: edit temp removed, config intact" '[ "$(temps)" = 0 ] && grep -q listen "$T/cfg/hs2-x.json"'

exit "$fail"

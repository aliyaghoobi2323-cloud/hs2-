#!/bin/bash
# tm_safe_apply (menu M1 hook): a binary that answers `apply --supported`
# takes the editor's save through `hs2 apply` (trial + auto-rollback); Enter
# confirms, "n" rolls back, a refused apply never confirms; a binary without
# safe apply keeps the classic tm_edit path. Driven through a real pty.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
printf '#!/bin/bash\nprintf "\\n" >> "$1"\n' > "$T/ed_change.sh"
# fake hs2: logs every call; SUPPORT=0 = an old binary, APPLYRC = apply's status
cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
case "$1 ${2:-}" in
  "apply --supported") [ "${SUPPORT:-1}" = 1 ] && { echo apply-v1; exit 0; }; echo "unknown command"; exit 2 ;;
  "apply -u") cp "${7}" "${5}"; exit "${APPLYRC:-0}" ;;
esac
exit 0
B
chmod +x "$T/ed_change.sh" "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T
CFG_DIR="$T/cfg"; mkdir -p "$CFG_DIR"; CFGF="$CFG_DIR/hs2-x.json"; echo '{"mode":"listen"}' > "$CFGF"
say(){ :; }; info(){ :; }; ok(){ :; }; warn(){ :; }; err(){ :; }; hr(){ :; }
systemctl(){ echo "systemctl $*" >> "$T/calls"; }; tm_healthy(){ return 0; }; tm_log_since(){ :; }; tm_validate(){ return 0; }
tm_editor(){ echo "$T/ed_change.sh"; }
tm_edit hs2-x "$CFGF"; echo "EDIT_DONE"
INNER
run(){ rm -rf "$T/cfg" "$T/calls"; printf "$1" | script -qec "T='$T' SUPPORT=${2:-1} APPLYRC=${3:-0} bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }

out=$(run '\n\n')
check "safe apply: goes through hs2 apply with a confirm timer" 'grep -q "^apply -u hs2-x -c .* --confirm-within 5m" "$T/calls"'
check "safe apply: Enter confirms" 'grep -q "^apply --confirm -u hs2-x" "$T/calls" && echo "$out" | grep -q EDIT_DONE'
check "safe apply: no direct systemctl restart" '! grep -q "systemctl restart" "$T/calls"'
check "safe apply: edit temp removed" '[ "$(find "$T/cfg" -name ".hs2-edit.*" | wc -l)" = 0 ]'
out=$(run '\nn\n')
check "safe apply: n rolls back" 'grep -q "^rollback -u hs2-x" "$T/calls" && ! grep -q "apply --confirm" "$T/calls"'
out=$(run '\n\n' 1 1)
check "safe apply refused: never confirmed, menu survives" '! grep -q "apply --confirm" "$T/calls" && echo "$out" | grep -q EDIT_DONE'
out=$(run '\n\n' 0)
check "old binary: classic path (restart, no apply)" 'grep -q "systemctl restart hs2-x" "$T/calls" && ! grep -q "^apply -u" "$T/calls"'
exit "$fail"

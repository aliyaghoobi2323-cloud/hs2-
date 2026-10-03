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
#   - the operator's own editor (SUDO_EDITOR / VISUAL / EDITOR) is used when it
#     is a terminal editor that exists here; desktop/missing ones are skipped.
#   - the edit happens in a private directory, so editor side files (vim's
#     .swp, emacs's ~ and #autosave#) — copies of the key — go with it; a REAL
#     vim is driven through the pty, including a kill -9 mid-edit.
#   - an editor exiting non-zero (vim's :cq) does not apply a change silently.
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
# side files like vim/emacs leave beside the file being edited, then a change
printf '#!/bin/bash\nd=$(dirname "$1"); b=$(basename "$1")\ncp "$1" "$d/.$b.swp"; cp "$1" "$1~"; cp "$1" "$d/#$b#"\nprintf "\\n" >> "$1"\n' > "$T/ed_side.sh"
# a change, then a non-zero exit (what vim's :cq does)
printf '#!/bin/bash\nprintf "\\n" >> "$1"\nexit 1\n' > "$T/ed_cq.sh"
chmod +x "$T/ed_change.sh" "$T/ed_kill.sh" "$T/ed_side.sh" "$T/ed_cq.sh"

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
  side)     tm_editor(){ echo "$T/ed_side.sh"; } ;;
  cq)       tm_editor(){ echo "$T/ed_cq.sh"; } ;;
  own)      : ;;   # the real tm_editor: EDITOR/VISUAL/SUDO_EDITOR from the environment
esac
outer(){ tm_tunnel_menu hs2-x; echo "BACK_IN_CALLER"; }
outer
INNER

run(){ rm -rf "$T/cfg"; printf "$2" | script -qec "T='$T' SCEN=$1 bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
temps(){ find "$T/cfg" -name '.hs2-edit.*' 2>/dev/null | wc -l; }
# anything in the config dir besides the config and its .prev = a leaked copy
strays(){ find "$T/cfg" -mindepth 1 ! -name hs2-x.json ! -name hs2-x.json.prev 2>/dev/null | wc -l; }

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

# ---- editor side files and exit status ---------------------------------------
out=$(run side '4\n\n0\n')
check "editor side files (.swp, ~, #autosave#) leave nothing behind after apply" '[ "$(strays)" = 0 ] && echo "$out" | grep -q BACK_IN_CALLER'
out=$(run cq '4\n\nn\n0\n')
check "editor exits non-zero, answer N: change NOT applied, nothing left" '[ "$(wc -l < "$T/cfg/hs2-x.json")" = 1 ] && [ "$(strays)" = 0 ] && echo "$out" | grep -q BACK_IN_CALLER'
out=$(run cq '4\n\ny\n0\n')
check "editor exits non-zero, answer y: change applied" '[ "$(wc -l < "$T/cfg/hs2-x.json")" = 2 ] && [ "$(strays)" = 0 ]'

# ---- which editor (no pty needed) --------------------------------------------
pick(){ env -i PATH="$PATH" "$@" bash -c 'source "$1/core.sh" >/dev/null 2>&1; info(){ echo "INFO: $*" >&2; }
        echo "PICK=[$(tm_user_editor verbose)] LABEL=[$(tm_editor_label)]"' _ "$T" 2>&1; }
out=$(pick); check "no editor set: nothing picked, label nano" 'echo "$out" | grep -qF "PICK=[] LABEL=[nano]"'
out=$(pick EDITOR=vi); check "EDITOR=vi is used" 'echo "$out" | grep -qF "PICK=[vi] LABEL=[vi]"'
out=$(pick EDITOR=vi VISUAL=nano); check "VISUAL wins over EDITOR" 'echo "$out" | grep -qF "PICK=[nano]"'
out=$(pick EDITOR=vi VISUAL=vi SUDO_EDITOR=nano); check "SUDO_EDITOR wins over both" 'echo "$out" | grep -qF "PICK=[nano]"'
out=$(pick VISUAL="code --wait" EDITOR=vi); check "desktop VISUAL skipped (explained), falls to EDITOR" 'echo "$out" | grep -qF "PICK=[vi]" && echo "$out" | grep -q "desktop editor"'
out=$(pick EDITOR=no-such-editor-xyz); check "missing editor skipped (explained), label nano" 'echo "$out" | grep -qF "PICK=[] LABEL=[nano]" && echo "$out" | grep -q "not installed"'
out=$(pick EDITOR="$(command -v vi) -n"); check "absolute path + arguments kept, label is the basename" 'echo "$out" | grep -qF "PICK=[$(command -v vi) -n] LABEL=[vi]"'
# Run where '*' would expand to exactly one name: an installed editor. A glob-
# expanding implementation would then pick it; the real one must not.
mkdir -p "$T/globonly" && : > "$T/globonly/vi"
out=$(cd "$T/globonly" && pick EDITOR='*'); check "EDITOR='*' is never glob-expanded into a command (even where it would match 'vi')" 'echo "$out" | grep -qF "PICK=[]"'
out=$(pick EDITOR=" vi"); check "leading space: label is the editor that actually opens" 'echo "$out" | grep -qF "LABEL=[vi]"'

# ---- the startup sweep: leftovers go, another session's live edit stays -----
sleep 300 & LIVE=$!                       # stands in for a second installer session
DEAD=999999; while [ -d "/proc/$DEAD" ]; do DEAD=$((DEAD - 1)); done
mkdir -p "$T/sw/.hs2-edit.$DEAD.AAA111" "$T/sw/.hs2-edit.$LIVE.CCC333"
echo key > "$T/sw/.hs2-edit.$DEAD.AAA111/hs2-x.json"; echo key > "$T/sw/.hs2-edit.$LIVE.CCC333/hs2-x.json"
echo key > "$T/sw/.hs2-edit.BBB222"     # an older version's plain temp file
echo '{}' > "$T/sw/hs2-x.json"
sed "s#^CFG_DIR=/etc/hs2#CFG_DIR=$T/sw#" "$T/core.sh" > "$T/core_sw.sh"
bash -c 'source "$1" >/dev/null 2>&1' _ "$T/core_sw.sh"
check "startup sweep removes a dead session's edit dir and an old-style file" '[ ! -e "$T/sw/.hs2-edit.$DEAD.AAA111" ] && [ ! -e "$T/sw/.hs2-edit.BBB222" ] && [ -f "$T/sw/hs2-x.json" ]'
check "startup sweep leaves a LIVE session's edit dir alone" '[ -f "$T/sw/.hs2-edit.$LIVE.CCC333/hs2-x.json" ]'
# A REUSED PID: the process is alive, but it started after the leftover was last
# touched (a reboot after a power loss gave the dead installer's PID to some
# daemon) — it cannot be the owner, so the leftover is swept.
mkdir -p "$T/sw/.hs2-edit.$LIVE.DDD444"; echo key > "$T/sw/.hs2-edit.$LIVE.DDD444/hs2-x.json"
touch -d '2 hours ago' "$T/sw/.hs2-edit.$LIVE.DDD444"
bash -c 'source "$1" >/dev/null 2>&1' _ "$T/core_sw.sh"
check "startup sweep removes a leftover whose PID was reused by a newer process" '[ ! -e "$T/sw/.hs2-edit.$LIVE.DDD444" ] && [ -f "$T/sw/.hs2-edit.$LIVE.CCC333/hs2-x.json" ]'
kill "$LIVE" 2>/dev/null; wait "$LIVE" 2>/dev/null
bash -c 'source "$1" >/dev/null 2>&1' _ "$T/core_sw.sh"
check "…and sweeps the live one once that session is gone" '[ -z "$(find "$T/sw" -name ".hs2-edit.*")" ]'

# ---- a REAL vim, driven through the pty ---------------------------------------
if command -v vim >/dev/null 2>&1; then
  VIMCMD="vim -N -u NONE -i NONE"
  # Every real-vim run is bounded: a vim that never got its keys must not hang
  # the suite (a cold first start can be slow), and none may outlive the test.
  runv(){ rm -rf "$T/cfg"; printf "$1" | TERM=xterm timeout 40 script -qec "T='$T' SCEN=own EDITOR='$VIMCMD' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
          pkill -9 -f "^vim -N -u NONE -i NONE $T/" 2>/dev/null || true; }
  out=$(runv '4\n\nGA\n\033:wq\n')
  check "real vim (:wq): change applied, no swap/backup left" '[ "$(wc -l < "$T/cfg/hs2-x.json")" = 2 ] && [ "$(strays)" = 0 ] && echo "$out" | grep -q BACK_IN_CALLER'
  out=$(runv '4\n\nGA\n\033:cq\n')
  check "real vim (:cq): change NOT applied, nothing left" '[ "$(wc -l < "$T/cfg/hs2-x.json")" = 1 ] && [ "$(strays)" = 0 ] && echo "$out" | grep -q BACK_IN_CALLER'
  # A dropped SSH session mid-edit: vim dies (kill -9, so it cannot tidy its
  # .swp) and the installer gets SIGHUP. Neither the swap file nor the copy may
  # survive beside the config.
  ( for _ in $(seq 1 200); do sleep 0.1; pkill -9 -f "^vim -N -u NONE -i NONE $T/cfg/" && break; done
    sleep 1; pkill -HUP -f "bash $T/inner.sh" ) &
  out=$(runv '4\n\nihello')
  wait
  check "real vim killed + session dropped mid-edit: config intact, no swap/copy left" 'grep -q listen "$T/cfg/hs2-x.json" && [ "$(strays)" = 0 ]'
else
  echo "SKIP real-vim tests (vim not installed)"
fi

exit "$fail"

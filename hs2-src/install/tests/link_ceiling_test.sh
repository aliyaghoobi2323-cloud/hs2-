#!/bin/bash
# The auto link-pool ceiling in the installer:
#   - a NEW setup writes max_links 0 (auto) and says what it resolves to now;
#     it survives a missing/older binary under set -euo pipefail;
#   - the existing-install migration still writes a fixed 32 (never auto);
#   - the Link pool screen shows 'auto', accepts 'auto', warns about a fixed
#     value the hardware disagrees with, writes min/max in an order the binary's
#     per-set min<=max check accepts, and prints the daemon's exact ceiling line
#     (on every role, including a direct Kharej whose own values do nothing).
# Driven on a pty under the installer's real `set -euo pipefail`.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-400)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP link_ceiling_test (no script(1))"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"

# A fake hs2 that answers recommend-links like a high-profile box.
cat > "$T/hs2" <<'FAKE'
#!/bin/bash
[ "${1:-}" = recommend-links ] || exit 2
if [ "${2:-}" = --why ]; then echo "64  (high profile: 7.8 GB RAM, 4 core(s))"; else echo 64; fi
FAKE
chmod +x "$T/hs2"

# --- use_auto_link_ceiling --------------------------------------------------
ualc(){ # bin
  bash -c "source '$T/core.sh' >/dev/null 2>&1; info(){ echo \"INFO \$*\"; }; BIN='$1'; use_auto_link_ceiling; echo \"LINK_MAX=\$LINK_MAX\"" 2>&1
}
out=$(ualc "$T/hs2")
check "new setup: LINK_MAX=0 (auto)" 'echo "$out" | grep -qx "LINK_MAX=0"'
check "new setup: says auto and what it is now" 'echo "$out" | grep -q "auto (max_links 0)" && echo "$out" | grep -q "right now: 64  (high profile"'
check "new setup on a 64 box: no visibility note" '! echo "$out" | grep -q "parallel links between the two servers"'
cat > "$T/hs2big" <<'FAKE'
#!/bin/bash
[ "${1:-}" = recommend-links ] || exit 2
if [ "${2:-}" = --why ]; then echo "300  (16.6 GB RAM, 20 cores: one link per 48 MB of RAM, at most 300)"; else echo 300; fi
FAKE
chmod +x "$T/hs2big"
out=$(ualc "$T/hs2big")
check "new setup on a 300 box: the visibility note, once" '[ "$(echo "$out" | grep -c "up to 300 parallel links between the two servers")" = 1 ] && echo "$out" | grep -q "your call"'
out=$(ualc "$T/nonexistent-hs2")
check "missing binary: still auto, no abort under set -e" 'echo "$out" | grep -qx "LINK_MAX=0"'
check "missing binary: no 'right now' claim" '! echo "$out" | grep -q "right now"'
# An older hs2 (kept when a tunnel is added) prints "unknown command" on
# STDOUT and exits 2 — exactly like the real pre-auto binary.
cat > "$T/hs2old" <<'OLD'
#!/bin/bash
echo "unknown command"; exit 2
OLD
chmod +x "$T/hs2old"
out=$(ualc "$T/hs2old")
check "older binary: config still auto (0)" 'echo "$out" | grep -qx "LINK_MAX=0"'
check "older binary: never shows 'unknown command' as hardware" '! echo "$out" | grep -q "unknown command"'
check "older binary: says it runs auto as 32 until upgraded" 'echo "$out" | grep -q "predates it and runs it as a fixed 32 until hs2 is upgraded"'
out=$(grep -n "install_binary\|use_auto_link_ceiling" "$INST" | awk -F: '/install_binary$/ {b=$1} /use_auto_link_ceiling$/ && b && $1==b+1 {n++} END{print n+0}')
check "both setups call it right after install_binary" '[ "$out" = 2 ]'

# --- migration keeps a fixed 32 ----------------------------------------------
printf '{"mode":"dial","min_links": 8, "max_links": 16, "per_link": 8, "carrier":"mtcp"}\n' > "$T/old.json"
out=$(bash -c "source '$T/core.sh' >/dev/null 2>&1; ok(){ :; }; info(){ :; }; warn(){ :; }; LINK_MAX=0; CFG='$T/old.json'; BIN='$T/hs2'; migrate_config >/dev/null 2>&1; cat '$T/old.json'")
check "migration writes max_links 32 even when LINK_MAX is 0" 'echo "$out" | grep -q "\"max_links\": 32"'

# --- the Link pool screen ------------------------------------------------------
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh" >/dev/null 2>&1
say(){ printf '%s\n' "$*"; }; warn(){ say "$*"; }; info(){ say "$*"; }; ok(){ say "$*"; }; hr(){ :; }; pause(){ :; }
tm_cfgset(){ echo "CFGSET $2=$3"; }
tm_apply_restart(){ echo "APPLIED"; }
status_path(){ echo "$SPF"; }
BIN="$FAKEBIN"
tm_tune_links hs2-x "$CFGF"
echo "RETURNED"
INNER
run(){ # mode reverse maxfield keys [status-json]
  printf '{"mode":"%s","reverse":%s,"carrier":"mtcp","min_links":2,%s"per_link":8}\n' "$1" "$2" "$3" > "$T/c.json"
  rm -f "$T/st.json"; [ -n "${5:-}" ] && printf '%s\n' "$5" > "$T/st.json"
  printf "$4" | script -qec "T='$T' CFGF='$T/c.json' FAKEBIN='${FAKEBIN_OVERRIDE:-$T/hs2}' SPF='$T/st.json' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
now=$(date +%s)
live='{"updated":'"$now"',"cfg_max":64,"ceiling_text":"48 links — limited by the Kharej server (reverse: the lower of the two applies)"}'

out=$(run dial true '"max_links":0,' '\n\n\n')
check "auto config: shown as auto with today's value" 'echo "$out" | grep -q "max=auto, now 64"'
check "auto config: Enter keeps auto (writes 0)" 'echo "$out" | grep -q "CFGSET max_links=0" && echo "$out" | grep -q APPLIED'
out=$(run dial true '' '\n\n\n')
check "absent max_links: shown as the default 32" 'echo "$out" | grep -q "max=32 (default — not set)"'
check "absent max_links: Enter keeps 32 (never silently auto)" 'echo "$out" | grep -q "CFGSET max_links=32" && ! echo "$out" | grep -q "CFGSET max_links=0"'
check "absent max_links on a 64 box: offered auto" 'echo "$out" | grep -q "fixed at 32; this server.s hardware gives 64"'

out=$(FAKEBIN_OVERRIDE="$T/hs2old" run dial true '"max_links":0,' '\n\n\n')
check "older binary in the menu: no 'unknown command', honest auto line" '! echo "$out" | grep -q "unknown command" && echo "$out" | grep -q "runs it as 32 until upgraded"'

out=$(run dial false '"max_links":32,' '\nauto\n\n')
check "fixed 32 on a 64 box: told, offered auto" 'echo "$out" | grep -q "The ceiling is fixed at 32; this server.s hardware gives 64"'
check "typing auto writes 0" 'echo "$out" | grep -q "CFGSET max_links=0"'

out=$(run dial false '"max_links":32,' '40\nauto\n\n')
first=$(printf '%s\n' "$out" | grep -m1 -o "CFGSET [a-z_]*" || true)
check "min 40 + auto from fixed 32: max set FIRST (binary accepts every step)" '[ "$first" = "CFGSET max_links" ] && echo "$out" | grep -q "CFGSET min_links=40"'

out=$(run dial false '"max_links":32,' '\n40\n\n')
check "a number is still a fixed ceiling" 'echo "$out" | grep -q "CFGSET max_links=40"'
out=$(run dial false '"max_links":32,' '\nabc\n\n')
check "garbage max refused, nothing written" 'echo "$out" | grep -q "or .auto." && ! echo "$out" | grep -q CFGSET'
out=$(run dial false '"max_links":32,' '50\n40\n\n')
check "min above a fixed max refused" 'echo "$out" | grep -q "cannot be more than max" && ! echo "$out" | grep -q CFGSET'

out=$(run dial true '"max_links":0,' '\n\n\n' "$live")
check "running tunnel: the daemon's exact ceiling line is shown" 'echo "$out" | grep "Now:" | grep -q "48 links — limited by the Kharej server"'
out=$(run listen false '"max_links":0,' '\n' "$live")
check "direct Kharej: still shows the live ceiling, writes nothing" 'echo "$out" | grep "Now:" | grep -q "48 links" && ! echo "$out" | grep -q CFGSET'
stale='{"updated":1,"ceiling_text":"STALE LINE"}'
out=$(run dial true '"max_links":0,' '\n\n\n' "$stale")
check "a stale status file is not shown as live" '! echo "$out" | grep -q "STALE LINE"'
exit "$fail"

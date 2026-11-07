#!/bin/bash
# Step 13 installer fixes, tested in a sandbox (no root, no systemd):
#   apt_q        — apt is bounded: a hanging apt-get is killed by the timeout
#                  and never fails the caller;
#   verify_local — a hand-copied binary is checked against a .sha256 copied
#                  with it (match passes, mismatch fails, none passes + hash);
#   restore      — the old static sysctl file is not brought back when the
#                  binary tunes the kernel itself (checked on the source text).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
sed -n '/^apt_q(){/,/^}/p;/^verify_local(){/,/^}/p' "$INST" > "$T/fns.sh"
for f in apt_q verify_local; do grep -q "^$f(){" "$T/fns.sh" || { echo "FAIL $f not found in install.sh"; exit 1; }; done
cat > "$T/run.sh" <<RUN
set -euo pipefail
ok(){ echo "OK: \$*" >&2; }; warn(){ echo "WARN: \$*" >&2; }; err(){ echo "ERR: \$*" >&2; }; info(){ echo "INFO: \$*" >&2; }
BIN=/usr/local/bin/hs2
source "$T/fns.sh"
RUN

# ---- apt_q ---------------------------------------------------------------------
mkdir -p "$T/bin"
printf '#!/bin/sh\necho "$@" > %s/args\nsleep 30\n' "$T" > "$T/bin/apt-get"; chmod +x "$T/bin/apt-get"
s=$(date +%s)
out=$(PATH="$T/bin:$PATH" HS2_APT_TIMEOUT=2 bash -c "source '$T/run.sh'; apt_q update -q; echo AFTER" 2>&1)
d=$(( $(date +%s) - s ))
check "apt_q: a hanging apt-get is cut at HS2_APT_TIMEOUT (took ${d}s)" '[ "$d" -le 15 ]'
check "apt_q: never fails the caller (set -e script goes on)" 'echo "$out" | grep -qx AFTER'
check "apt_q: each fetch has a timeout" 'grep -q "Acquire::http::Timeout=15" "$T/args" && grep -q -- "update -q" "$T/args"'

# ---- verify_local ----------------------------------------------------------------
head -c 50000 /dev/urandom > "$T/hs2-linux-amd64"; cp "$T/hs2-linux-amd64" "$T/tmp"
vl(){ bash -c "source '$T/run.sh'; if verify_local '$T/tmp' '$T/hs2-linux-amd64.sha256'; then echo RC=0; else echo RC=\$?; fi" 2>&1; }
sha256sum "$T/hs2-linux-amd64" | sed "s#  .*#  hs2-linux-amd64#" > "$T/hs2-linux-amd64.sha256"
out=$(vl); check "verify_local: a copy matching its .sha256 passes" 'echo "$out" | grep -qx RC=0 && echo "$out" | grep -q matches'
sha256sum "$T/hs2-linux-amd64" | cut -d" " -f1 | tr a-f A-F > "$T/hs2-linux-amd64.sha256"
out=$(vl); check "verify_local: an upper-case bare hash also matches" 'echo "$out" | grep -qx RC=0'
printf x >> "$T/tmp"
out=$(vl); check "verify_local: a truncated or altered copy fails" 'echo "$out" | grep -qx RC=1 && echo "$out" | grep -q mismatch'
rm "$T/hs2-linux-amd64.sha256"
out=$(vl); check "verify_local: no .sha256 passes and prints the hash to compare" 'echo "$out" | grep -qx RC=0 && echo "$out" | grep -qE "[0-9a-f]{64}"'
echo garbage > "$T/hs2-linux-amd64.sha256"
out=$(vl); check "verify_local: a garbage .sha256 counts as none" 'echo "$out" | grep -qx RC=0 && echo "$out" | grep -q "No usable"'

# ---- restore: no stale sysctl file -------------------------------------------------
body=$(sed -n '/^restore(){/,/^}/p' "$INST")
check "restore: removes the old 99-hs2.conf when the binary has 'tune'" 'echo "$body" | grep -q "\"\$BIN\" tune -h" && echo "$body" | grep -q "rm -f /etc/sysctl.d/99-hs2.conf"'
check "restore: no unconditional sysctl -p of the old file" '! echo "$body" | grep -q "^  \[ -f /etc/sysctl.d/99-hs2.conf \] && sysctl -p"'
check "install_prereqs: no unbounded apt-get left" '! sed -n "/^install_prereqs(){/,/^}/p" "$INST" | grep -q "^  apt-get"'
exit $fail

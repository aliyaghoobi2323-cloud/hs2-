#!/bin/bash
# The installer gives every builtin-cover tunnel a per-install cover_seed, so
# its cover page stops being byte-identical to every other install:
#   - every setup heredoc that writes "backend_addr": "builtin" also writes a
#     cover_seed (static check on install.sh);
#   - the seed is independent of the shared key, and written as valid JSON;
#   - migrate_config adds a seed to an existing builtin tunnel that has none,
#     leaves a custom backend_addr alone, and never adds a second one.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
PY=$(command -v python3 || true)
valid_json(){ [ -n "$PY" ] || return 0; "$PY" -c 'import json,sys; json.load(open(sys.argv[1]))' "$1"; }

# --- static: every builtin-cover config heredoc carries a cover_seed ----------
nbuiltin=$(grep -c '"backend_addr": "builtin",' "$INST")
nseed=$(grep -c '"cover_seed": "\$COVER_SEED",' "$INST")
check "every builtin-cover setup heredoc writes a cover_seed ($nbuiltin builtin / $nseed seeded)" '[ "$nbuiltin" = "$nseed" ] && [ "$nbuiltin" -ge 4 ]'
# COVER_SEED is generated from openssl rand, NOT derived from the shared key.
check "cover_seed is generated independently (openssl rand), not from the key" 'grep -q "COVER_SEED=\$(openssl rand -hex 16)" "$INST" && ! grep -q "COVER_SEED=.*SHARED" "$INST"'

# --- migrate_config: extract it and the helpers it calls, run against sandbox -
sed -n '/^configure_renewal(){/,/^}/p;/^migrate_config(){/,/^}/p;/^cfg_field(){/,/^}/p;/^say(){/p;/^CERT_HOOK=/p;/^LE_DIR=/p;/^cert_lineage(){/,/^}/p;/^cert_conf_get(){/,/^}/p;/^cert_renew_method(){/,/^}/p;/^cert_until(){/,/^}/p' "$INST" \
 | sed "s#^LE_DIR=.*#LE_DIR=$T/le#" > "$T/fns.sh"
mkdir -p "$T/le/renewal"
run_migrate(){ # cfg-path
  bash -c "set -euo pipefail
    CFG='$1'; LINK_MIN=2; LINK_MAX=32; LINK_PER=8
    ok(){ :; }; info(){ :; }; warn(){ :; }; systemctl(){ :; }; modprobe(){ :; }
    source '$T/fns.sh'
    migrate_config" 2>&1
}

# a builtin-cover tunnel with no seed -> one is added, valid JSON, one only
printf '{\n  "mode": "listen", "carrier": "mtcp", "reverse": false,\n  "backend_addr": "builtin",\n  "shared_key": "abc123",\n  "expose": "127.0.0.1:8443"\n}\n' > "$T/a.json"
out=$(run_migrate "$T/a.json"); rc=$?
check "migrate: upgrade continues (exit 0)" '[ $rc = 0 ]'
check "migrate: a builtin tunnel with no seed gets one" 'grep -q "\"cover_seed\"" "$T/a.json"'
check "migrate: exactly one cover_seed added" '[ "$(grep -c "cover_seed" "$T/a.json")" = 1 ]'
check "migrate: result is valid JSON" 'valid_json "$T/a.json"'
seed1=$(grep -o '"cover_seed": "[0-9a-f]*"' "$T/a.json")
check "migrate: the seed is 32 hex chars" 'echo "$seed1" | grep -qE "\"cover_seed\": \"[0-9a-f]{32}\""'

# running migrate again must NOT add a second seed (idempotent)
cp "$T/a.json" "$T/a.before"
out=$(run_migrate "$T/a.json")
check "migrate: second run adds no second seed (idempotent)" '[ "$(grep -c "cover_seed" "$T/a.json")" = 1 ]'
check "migrate: second run leaves the seed unchanged" 'grep -qF "$(grep -o "\"cover_seed\": \"[0-9a-f]*\"" "$T/a.before")" "$T/a.json"'

# a custom backend_addr (the user's own site) must be left alone — no seed
printf '{\n  "mode": "listen", "carrier": "mtcp", "reverse": false,\n  "backend_addr": "127.0.0.1:8080",\n  "shared_key": "abc123"\n}\n' > "$T/b.json"
out=$(run_migrate "$T/b.json")
check "migrate: a custom backend_addr gets NO cover_seed" '! grep -q "cover_seed" "$T/b.json"'

# a config that already has a seed is untouched
printf '{\n  "backend_addr": "builtin", "cover_seed": "00112233445566778899aabbccddeeff",\n  "shared_key": "abc123"\n}\n' > "$T/c.json"
out=$(run_migrate "$T/c.json")
check "migrate: an existing seed is not duplicated" '[ "$(grep -c "cover_seed" "$T/c.json")" = 1 ]'
check "migrate: an existing seed is unchanged" 'grep -q "00112233445566778899aabbccddeeff" "$T/c.json"'

exit "$fail"

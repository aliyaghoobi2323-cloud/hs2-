#!/bin/bash
# rel50: the OLD "carrier": "reality" stays warning-only, and the warning is
# visible: the tunnel manager list prints it under such a tunnel (both
# languages), nothing for other carriers; the upgrade path (migrate_config)
# warns too. (hs2 doctor/check: cmd/hs2 sec41_test.go.)
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
sed -n '/^tm_legacy_reality(){/,/^}/p;/^nm_t(){/p' "$INST" > "$T/fns.sh"
printf '{\n  "mode": "listen", "carrier": "reality",\n  "addr": ":443"\n}\n' > "$T/old.json"
printf '{"mode":"listen","carrier":"l3mtcp","front":{"kind":"reality"}}\n' > "$T/new.json"
run(){ bash -c "C_R=; C_0=; NM_LANG=$2; say(){ printf '%s\n' \"\$*\"; }; . '$T/fns.sh'; tm_legacy_reality '$1'"; }
o=$(run "$T/old.json" en); f=$(run "$T/old.json" fa); n=$(run "$T/new.json" en)
check "tunnel list warns for carrier reality (en)" '[[ "$o" == *"old experimental carrier"* ]]'
check "tunnel list warns for carrier reality (fa)" '[[ "$f" == *"کریر قدیمی"* ]]'
check "no warning for a reality FRONT on l3mtcp" '[ -z "$n" ]'
check "tm_list calls it for every tunnel" 'sed -n "/^tm_list(){/,/^}/p" "$INST" | grep -q "tm_legacy_reality \"\$cfg\""'
check "upgrade (migrate_config) warns, does not rewrite" 'sed -n "/^migrate_config(){/,/^}/p" "$INST" | grep -q "OLD experimental carrier"'
exit $fail

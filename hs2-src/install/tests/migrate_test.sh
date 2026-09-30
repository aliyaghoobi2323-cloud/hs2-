#!/bin/bash
# Regression test for the installer's upgrade migration, runnable anywhere
# (no systemd, no root): it extracts migrate_config/configure_renewal from
# install.sh and runs them under the installer's own `set -euo pipefail`
# against a sandbox, including the case that took a live tunnel down — a second
# certbot lineage (e.g. a panel's certificate) that this tunnel does not use.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

mkdir -p "$T/renewal"
printf '{\n  "cert_file": "/etc/letsencrypt/live/a.example/fullchain.pem",\n  "min_links": 8, "max_links": 16, "per_link": 8,\n  "x": 1\n}\n' > "$T/cfg.json"
printf 'version = 2.9.0\narchive_dir = /x\n\n[renewalparams]\nauthenticator = standalone\nrenew_hook = systemctl restart hs2\n' > "$T/renewal/a.example.conf"
printf 'version = 2.9.0\n[renewalparams]\nauthenticator = standalone\n' > "$T/renewal/z-panel.example.conf"
cp "$T/renewal/z-panel.example.conf" "$T/panel.orig"
: > "$T/sysctl.conf"   # an old static sysctl file that must be removed

# migrate_config also calls the installer helpers cfg_field and say; extract
# them too (say is a one-liner, so it is matched as a single line — a range
# would run on into the functions after it).
sed -n '/^configure_renewal(){/,/^}/p;/^migrate_config(){/,/^}/p;/^cfg_field(){/,/^}/p;/^say(){/p' "$INST" \
 | sed "s#/etc/letsencrypt/renewal#$T/renewal#g; s#/etc/modules-load.d/hs2.conf#/dev/null#; s#/etc/sysctl.d/99-hs2.conf#$T/sysctl.conf#g" > "$T/fns.sh"
cat > "$T/run.sh" <<RUN
set -euo pipefail
CFG=$T/cfg.json; LINK_MIN=2; LINK_MAX=32; LINK_PER=8
ok(){ :; }; info(){ :; }; warn(){ :; }; systemctl(){ :; }; modprobe(){ :; }
source $T/fns.sh
migrate_config
echo REACHED_END
RUN
out=$(bash "$T/run.sh" 2>&1); rc=$?
check "upgrade continues past migrate_config with a foreign certbot lineage" '[ $rc = 0 ] && echo "$out" | grep -x REACHED_END >/dev/null'
check "fixed 8/16/8 pool migrated to 2/32/8" 'grep -F "\"min_links\": 2, \"max_links\": 32, \"per_link\": 8," "$T/cfg.json" >/dev/null'
check "config is still valid JSON" 'python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$T/cfg.json"'
check "renew_hook switched to reload (in place)" 'grep -x "renew_hook = systemctl reload hs2" "$T/renewal/a.example.conf" >/dev/null && ! grep -q "restart hs2" "$T/renewal/a.example.conf"'
check "renew_before_expiry is top-level (before [renewalparams])" '[ "$(grep -n -m1 "^renew_before_expiry = 30 days" "$T/renewal/a.example.conf" | cut -d: -f1)" -lt "$(grep -n -m1 "^\[renewalparams\]" "$T/renewal/a.example.conf" | cut -d: -f1)" ]'
check "the panel's own lineage is left untouched" 'cmp -s "$T/panel.orig" "$T/renewal/z-panel.example.conf"'
check "old static sysctl file removed" '[ ! -e "$T/sysctl.conf" ]'
# idempotent: running the migration a second time changes nothing and still succeeds
cp "$T/cfg.json" "$T/cfg.1"; cp "$T/renewal/a.example.conf" "$T/a.1"
out=$(bash "$T/run.sh" 2>&1); rc=$?
check "second run succeeds and is a no-op" '[ $rc = 0 ] && cmp -s "$T/cfg.1" "$T/cfg.json" && cmp -s "$T/a.1" "$T/renewal/a.example.conf"'
# an install from the 7-day era moves to 30 days in place (one line, not two)
sed -i 's/^renew_before_expiry = 30 days/renew_before_expiry = 7 days/' "$T/renewal/a.example.conf"
out=$(bash "$T/run.sh" 2>&1); rc=$?
check "old 7-day renewal window moved to 30 days in place" '[ $rc = 0 ] && [ "$(grep -c "^renew_before_expiry" "$T/renewal/a.example.conf")" = 1 ] && grep -x "renew_before_expiry = 30 days" "$T/renewal/a.example.conf" >/dev/null'
# no certbot at all (typical kharej)
rm -rf "$T/renewal"; mkdir "$T/renewal"
out=$(bash "$T/run.sh" 2>&1); rc=$?
check "no certbot lineages at all: still continues" '[ $rc = 0 ]'
exit $fail

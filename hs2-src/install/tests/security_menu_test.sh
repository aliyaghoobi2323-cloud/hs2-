#!/bin/bash
# Menu M5 (= Reality phase 5), screen 4 "Security & camouflage"
# (`hs2-menu --new` -> 4), driven through a real pty under the installer's
# `set -euo pipefail` with a fake hs2 that logs its calls: per-IP front state,
# front tls / reality / both (target, advanced server name) under the admin
# lock, target picker and one-target check (read-only), key rotation (all /
# one IP), keygen, the doctor's front category, certificate / config editor
# kept classic, fa texts, a narrow non-UTF-8 TTY, and the classic screen for
# an old binary.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"

cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
[ "${OLDBIN:-0}" = 1 ] && [ "$1" = reality ] && [ "${2:-}" = --supported ] && { echo "usage: hs2 reality keygen"; exit 2; }
case "$1 ${2:-}" in
  "dashboard --header") echo "testhost · 1 tunnel"; exit 0 ;;
  "reality --supported") echo "front rotate-key status targets check-target keygen"; exit 0 ;;
  "reality status") echo "hs2-a — running — listening side: the fronts are chosen here"
                    echo "IP               config     in use   target                     links: reality/tls/cover/rejected"
                    echo "1.1.1.1          both       both     www.site.ir:443            12/3/1/0"; exit 0 ;;
  "reality targets") echo "Listener: the IRAN server (reverse mode: kharej dials into it)."; echo "1   www.digikala.com:443       ACCEPT  56     match IR AS43211 Digikala side OK"; echo "2   www.hetzner.com:443        REJECT  236    mismatch DE AS24940      MISMATCH: the Iran server listens"; echo "3   www.isna.ir:443            REJECT  84     match IR AS205585        on ArvanCloud, a multi-tenant CDN"; exit 0 ;;
  "reality check-target") echo "VERDICT: ACCEPT — usable as a reality target"; exit 0 ;;
  "reality front") echo "~ lb.kharej[0].front.kind: tls -> reality"; exit 0 ;;
  "doctor --deep") echo "[OK  ] front hs2-a: reality target www.site.ir:443 reachable"; exit 0 ;;
  "config lock") echo locked; exit 0 ;;
esac
exit 0
B
chmod +x "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T OLDBIN
MENU_CONF="$T/menu.conf"; UNIT_DIR="$T/units"; mkdir -p "$UNIT_DIR"
printf 'lang=%s\nmode=%s\ncolor=%s\n' "${MLANG:-en}" "${MODE:-simple}" "${MCOLOR:-off}" > "$MENU_CONF"
hr(){ :; }
tm_units(){ echo hs2-a; }; tm_cfg(){ echo "$T/a.json"; }; unit_desc(){ echo "test"; }
systemctl(){ return 1; }
tm_cert_side(){ return 0; }; tm_cert(){ echo "CLASSIC-CERT" >> "$T/calls"; }; tm_edit(){ echo "CLASSIC-EDIT" >> "$T/calls"; }
nm_main
echo "MENU_DONE"
INNER
run(){ rm -f "$T/calls"; printf "$1" | script -qec "T='$T' OLDBIN=${OLDBIN:-0} MODE=${MODE:-simple} MLANG=${MLANG:-en} MCOLOR=${MCOLOR:-off} LANG=${XLANG:-en_US.UTF-8} LC_ALL= LC_CTYPE= COLUMNS=${COLS:-80} bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
locked(){ # the call happened between a lock and an unlock
  awk -v c="$1" '/^config lock/{l=1} /^config unlock/{l=0} index($0,c)==1{print (l?"IN":"OUT")}' "$T/calls" | grep -qx IN; }

out=$(run '4\n0\n0\n')
check "screen 4 shows the per-IP front state (hs2 reality status)" 'grep -q "^reality status -u hs2-a --lang en" "$T/calls" && echo "$out" | grep -q "12/3/1/0" && echo "$out" | grep -q "Security & camouflage"'
check "simple mode hides one-IP rotation / keygen / config editor" '! echo "$out" | grep -q "Rotate the key of one IP" && ! echo "$out" | grep -q "New key pair"'
check "menu exits cleanly" '! echo "$out" | grep -q "stopped unexpectedly" && ! echo "$out" | grep -q "unbound"'
out=$(run '4\n2\n1.1.1.1\n2\nwww.site.ir\n\n0\n0\n')
check "front reality with a target, under the admin lock" 'grep -q "^reality front -u hs2-a --ip 1.1.1.1 reality --target www.site.ir --lang en" "$T/calls" && locked "reality front"'
out=$(run '4\n2\n1.1.1.1\n3\n\n\n0\n0\n')
check "front both keeps the current target (Enter)" 'grep -q "^reality front -u hs2-a --ip 1.1.1.1 both --lang en" "$T/calls"'
out=$(run '4\n2\n1.1.1.1\n1\n\n0\n0\n')
check "front tls asks no target" 'grep -q "^reality front -u hs2-a --ip 1.1.1.1 tls --lang en" "$T/calls"'
out=$(run '4\n2\n1.1.1.1\n7\n0\n0\n')
check "invalid front choice does nothing" '! grep -q "^reality front" "$T/calls"'
out=$(run '4\n2\n\n0\n0\n')
check "empty IP does nothing" '! grep -q "^reality front" "$T/calls"'
out=$(run '4\n3\n\n\n4\ngithub.com\n\n0\n0\n')
check "target picker and one-target check (read-only, no lock)" 'grep -q "^reality targets -u hs2-a --lang en" "$T/calls" && grep -q "^reality check-target github.com -u hs2-a --lang en" "$T/calls" && echo "$out" | grep -q "depends on which server LISTENS" && echo "$out" | grep -q "MISMATCH" && echo "$out" | grep -q "REJECT" && ! grep -q "^config lock" "$T/calls"'
out=$(run '4\n3\nmy.site.ir\n\n0\n0\n')
check "picker with extra candidates" 'grep -q "^reality targets -u hs2-a --add my.site.ir --lang en" "$T/calls"'
out=$(run '4\n5\n\n6\n\n0\n0\n')
check "rotate all keys under the lock; doctor front category" 'locked "reality rotate-key -u hs2-a --all" && grep -q "^doctor --deep --fixes --only front" "$T/calls"'
out=$(run '4\n7\n0\n0\n')
check "certificate keeps the classic screen" 'grep -q "^CLASSIC-CERT" "$T/calls"'
out=$(MODE=advanced run '4\n2\n1.1.1.1\n2\nwww.site.ir\nalt.site.ir\n\n8\n1.1.1.1\n\n9\n\ne\n0\n0\n')
check "advanced: server name, one-IP rotation, keygen, config editor" 'grep -q "^reality front -u hs2-a --ip 1.1.1.1 reality --target www.site.ir --sni alt.site.ir --lang en" "$T/calls" && locked "reality rotate-key -u hs2-a --ip 1.1.1.1" && grep -q "^reality keygen --lang en" "$T/calls" && grep -q "^CLASSIC-EDIT" "$T/calls"'
out=$(MLANG=fa run '4\n3\n\n\n0\n0\n')
check "fa texts" 'echo "$out" | grep -q "امنیت و استتار" && echo "$out" | grep -q "پوشش یک IP" && echo "$out" | grep -q "به سرورِ شنونده بستگی دارد" && grep -q "^reality targets -u hs2-a --lang fa" "$T/calls" && grep -q "^reality status -u hs2-a --lang fa" "$T/calls"'
out=$(XLANG=C COLS=40 run '4\nx\n0\n0\n')
check "narrow non-UTF-8 TTY: invalid choice survives" 'echo "$out" | grep -q "Invalid choice" && [ "$(echo "$out" | grep -c "Security & camouflage")" -ge 2 ]'
out=$(OLDBIN=1 run '4\n2\n0\n')
check "old binary: classic security screen" 'grep -q "^CLASSIC-EDIT" "$T/calls" && ! grep -q "^reality status" "$T/calls"'
out=$(run '7\n7\n\n0\n0\n')
check "doctor screen 7) fronts category" 'grep -q "^doctor --deep --fixes --only front" "$T/calls"'

[ $fail = 0 ] && echo "ALL PASS" || echo "SOME FAILED"
exit $fail

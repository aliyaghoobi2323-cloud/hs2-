#!/bin/bash
# Menu M4, screens 3 "Load balancing & IPs" and 5 "Speed & performance"
# (`hs2-menu --new` → 3 / 5), driven through a real pty under the installer's
# `set -euo pipefail` with a fake hs2 that logs its calls: the live path table,
# cut history, IP suggestion, add/remove IP (advanced: SNI/cert), equal split,
# weights editor + mode (advanced only), park/unpark, presets, feature
# toggles, cores, kernel budget, speed test, the admin lock around every
# changing action, and the fallback to the classic screens for an old binary.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"

cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
[ "${OLDBIN:-0}" = 1 ] && { [ "$1" = lb ] || [ "$1" = perf ]; } && { echo "unknown command"; exit 2; }
case "$1 ${2:-}" in
  "dashboard --header") echo "testhost · 1 tunnel"; exit 0 ;;
  "lb --supported") echo lb-v1; exit 0 ;;
  "perf --supported") echo perf-v1; exit 0 ;;
  "lb show") echo "lb: auto · iran: 1.1.1.1=50 2.2.2.2=50"; echo "path  share/eff  links  users  Mbit  loss  RTT ms  auto  state  cuts 24h"
             echo "1.1.1.1 > 9.9.9.9  50%/60%  10/10  20  ↓50.0 ↑5.0  ↓0.3%  80/95  ×1.00 best  primary  0"; exit 0 ;;
  "lb suggest") echo "★ 2.2.2.2  → 9.9.9.9  70.1 ms  loss 0%"; echo "Suggested primary: 2.2.2.2"; exit 0 ;;
  "perf show") echo "preset: production"; echo "HS2_SMUX_AUTOWIN  1  default  window grows"; exit 0 ;;
  "perf presets") echo "production  recommended"; echo "low-cpu  less CPU"; exit 0 ;;
  "perf nic") echo "NIC eth0: 1 queue(s); rx interrupts on core 0 (100%)"; exit 0 ;;
  "config lock") echo locked; exit 0 ;;
esac
exit 0
B
chmod +x "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T OLDBIN
MENU_CONF="$T/menu.conf"; UNIT_DIR="$T/units"; mkdir -p "$UNIT_DIR"
printf 'lang=%s\nmode=%s\ncolor=off\n' "${MLANG:-en}" "${MODE:-simple}" > "$MENU_CONF"
hr(){ :; }
tm_units(){ echo hs2-a; }; tm_cfg(){ echo "$T/a.json"; }; unit_desc(){ echo "test"; }
systemctl(){ return 1; }
lb_supported(){ return 0; }; tm_has_ports(){ return 1; }
tm_lb(){ echo "CLASSIC-LB" >> "$T/calls"; }; tm_tune(){ echo "CLASSIC-TUNE" >> "$T/calls"; }
nm_main
echo "MENU_DONE"
INNER
run(){ rm -f "$T/calls"; printf "$1" | script -qec "T='$T' OLDBIN=${OLDBIN:-0} MODE=${MODE:-simple} MLANG=${MLANG:-en} LANG=en_US.UTF-8 LC_ALL= LC_CTYPE= bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
locked(){ # the call happened between a lock and an unlock
  awk -v c="$1" '/^config lock/{l=1} /^config unlock/{l=0} index($0,c)==1{print (l?"IN":"OUT")}' "$T/calls" | grep -qx IN; }

# ---- screen 3 -------------------------------------------------------------
out=$(run '3\n0\n0\n')
check "LB screen shows the live path table via hs2 lb show" 'grep -q "^lb show -u hs2-a --lang en" "$T/calls" && echo "$out" | grep -q "share/eff" && echo "$out" | grep -q "×1.00 best"'
check "simple mode hides the weights editor and mode" '! echo "$out" | grep -q "Weights editor" && ! echo "$out" | grep -q "Mode fixed / auto"'
check "menu exits cleanly" '! echo "$out" | grep -q "stopped unexpectedly" && ! echo "$out" | grep -q "unbound"'
out=$(run '3\n2\n\n3\n\n0\n0\n')
check "cut history and IP suggestion (read-only, no lock)" 'grep -q "^lb history -u hs2-a" "$T/calls" && grep -q "^lb suggest -u hs2-a" "$T/calls" && echo "$out" | grep -q "Suggested primary: 2.2.2.2" && ! grep -q "^config lock" "$T/calls"'
out=$(run '3\n4\n3.3.3.3\n\n5\n2.2.2.2\n\n6\n\n0\n0\n')
check "add IP under the admin lock" 'grep -q "^lb add-ip -u hs2-a --ip 3.3.3.3 --lang en" "$T/calls" && locked "lb add-ip"'
check "remove IP and split equally under the lock" 'locked "lb remove-ip -u hs2-a --ip 2.2.2.2" && locked "lb weights -u hs2-a --equal"'
out=$(run '3\n4\n\n0\n0\n')
check "add IP with an empty answer does nothing" '! grep -q "^lb add-ip" "$T/calls"'
out=$(run '3\n7\n2.2.2.2\n6\n\n8\n2.2.2.2\n\n0\n0\n')
check "park for N hours and unpark under the lock" 'locked "lb park -u hs2-a --ip 2.2.2.2 --hours 6" && locked "lb unpark -u hs2-a --ip 2.2.2.2"'
out=$(MODE=advanced run '3\n4\n3.3.3.3\nsni.example\n/c.pem\n/k.pem\n\n9\n1.1.1.1=70,2.2.2.2=30\n\nm\nfixed\n\n0\n0\n')
check "advanced: add IP with per-IP SNI/cert/key + front note" 'grep -q "^lb add-ip -u hs2-a --ip 3.3.3.3 --sni sni.example --cert /c.pem --key /k.pem --lang en" "$T/calls" && echo "$out" | grep -q "switch to reality / both in 4) Security"'
check "advanced: weights editor and mode fixed under the lock" 'locked "lb weights -u hs2-a 1.1.1.1=70,2.2.2.2=30" && locked "lb mode -u hs2-a fixed"'
out=$(run '3\n9\n0\n0\n')
check "simple mode: hidden items do nothing" '! grep -q "^lb weights" "$T/calls"'
out=$(MLANG=fa run '3\n0\n0\n')
check "Persian screen" 'echo "$out" | grep -q "لودبالانس و IPها" && grep -q "^lb show -u hs2-a --lang fa" "$T/calls"'

# ---- screen 5 -------------------------------------------------------------
out=$(run '5\n0\n0\n')
check "speed screen shows the effective keys via hs2 perf show" 'grep -q "^perf show -u hs2-a --lang en" "$T/calls" && echo "$out" | grep -q "preset: production"'
out=$(MODE=advanced run '5\n0\n0\n')
check "advanced mode shows every key" 'grep -q "^perf show -u hs2-a --advanced --lang en" "$T/calls"'
out=$(run '5\n1\nlow-cpu\n\n0\n0\n')
check "preset: list, then apply under the lock" 'grep -q "^perf presets" "$T/calls" && locked "perf preset -u hs2-a low-cpu"'
out=$(run '5\n1\n\n0\n0\n')
check "preset with an empty answer does nothing" '! grep -q "^perf preset " "$T/calls"'
out=$(run '5\n2\nHS2_LB_FLAP_PEN=0\n\n2\n-HS2_LB_FLAP_PEN\n\n0\n0\n')
check "feature toggle set / unset under the lock" 'locked "perf set -u hs2-a HS2_LB_FLAP_PEN=0" && locked "perf unset -u hs2-a HS2_LB_FLAP_PEN"'
out=$(run '5\n3\n0\n\n3\nc\n\n3\nx\n0\n0\n')
check "cores: NIC view, keep core 0, clear; junk ignored" 'grep -q "^perf nic" "$T/calls" && locked "perf cores -u hs2-a --keep 0" && locked "perf cores -u hs2-a --clear" && [ "$(grep -c "^perf cores" "$T/calls")" = 2 ]'
out=$(run '5\n4\ny\n\n4\nr\n\n4\n\n0\n0\n')
check "kernel: budget 600 (measured) and revert under the lock" 'locked "perf kernel --budget 600 --yes" && locked "perf kernel --revert-budget" && [ "$(grep -c "^perf kernel --budget" "$T/calls")" = 1 ]'
out=$(run '5\n5\n15201\n\n5\n\n0\n0\n')
check "speed test only with a port (prerequisites shown first)" 'grep -q "^perf speedtest --port 15201 --lang en" "$T/calls" && [ "$(grep -c "^perf speedtest --port" "$T/calls")" = 1 ]'

# ---- old binary -------------------------------------------------------------
out=$(OLDBIN=1 run '3\n0\n')
check "old binary: LB falls back to the classic screen" 'echo "$out" | grep -q "arrives in M4" && grep -q "^CLASSIC-LB" "$T/calls"'
out=$(OLDBIN=1 run '5\n1\n0\n')
check "old binary: speed falls back to the classic items" 'echo "$out" | grep -q "arrives in M4" && grep -q "^CLASSIC-TUNE" "$T/calls"'

[ $fail = 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }

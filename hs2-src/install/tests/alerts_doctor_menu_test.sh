#!/bin/bash
# Menu M6, screens 6 "Monitoring & alerts" and 7 "Server doctor"
# (`hs2-menu --new` → 6 / 7), driven through a real pty under the installer's
# `set -euo pipefail` with a fake hs2 that logs its calls: settings shown by
# hs2 alert conf show, the bot token read hidden and passed on stdin (never in
# argv or on screen), chat id / proxy / language / thresholds / quiet hours /
# daily summary, one alert kind on/off, per-tunnel timers, history, summary
# now, Telegram reachability; the doctor per category, numbered fixes applied
# through `hs2 doctor --fix N` under the admin lock (park asks for hours, the
# alerts fix opens screen 6), the report file, non-root and old-binary paths.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
# no pipefail inside a check: `echo "$out" | grep -q X` exits grep at the first
# match and the echo then dies of SIGPIPE, which pipefail would turn into a
# false FAIL on a busy host (seen once under parallel load)
fail=0; check(){ if (set +o pipefail; eval "$2"); then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP (script(1) not installed)"; exit 0; }
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
TOKEN=987654321:SecretTokenValueXYZ_abcdefghijklm

cat > "$T/hs2" <<'B'
#!/bin/bash
echo "$*" >> "$T/calls"
[ "${OLDBIN:-0}" = 1 ] && [ "${2:-}" = --supported ] && { echo "flag provided but not defined: -supported"; exit 2; }
case "$1 ${2:-}" in
  "dashboard --header") echo "testhost · 2 tunnels"; exit 0 ;;
  "alert --supported"|"doctor --supported") exit 0 ;;
  "alert conf")
    case "${3:-}" in
      token) cat > "$T/token"; echo "bot token saved (hidden)"; exit 0 ;;
      set) echo "saved"; exit 0 ;;
    esac
    echo "  bot token:       set (hidden)"; echo "  quiet hours:     23:00-07:00"
    echo "  alert kinds (thresholds):"; echo "    8) rtt      OFF    rtt_factor=1.6"; echo "    9) psi      on     cpu_pressure_pct=25"; exit 0 ;;
  "alert telegram-check") echo "Telegram (api.telegram.org) is NOT reachable from this server (typical on Iran servers): set a proxy"; exit 1 ;;
  "alert history") echo "2026-11-10 01:00:00 [testhost/hs2-a] rtt high"; echo "  1 alert line(s); per day: 11-10:1"; exit 0 ;;
  "alert summary-now") echo "summary sent for 2 tunnel(s)"; exit 0 ;;
  "doctor --deep")
    case "$*" in *--report*) f=$(echo "$*" | sed 's/.*--report \([^ ]*\).*/\1/'); echo "report" > "$f"; exit 0 ;; esac
    echo "[WARN] NIC: eth0 1 rx queue"; echo "       fix: 1) keep core 0 free for the hs2 tunnel units"
    echo "[WARN] path quality 2.2.2.2 → 9.9.9.9: loss"; echo "       fix: 2) park 2.2.2.2 on hs2-a for N hours"
    echo "[INFO] monitoring: alerts are off for: hs2-b"; echo "       fix: 3) open the alert setup (menu 6)"
    echo "[WARN] x-ui: 40 removes"; echo "       do:  advice only"
    printf '{"at":1,"fixes":[{"kind":"rxcore"},{"kind":"park"},{"kind":"alerts"}]}' > "$T/fixes.json"; exit 0 ;;
  "doctor --fix") echo "preview — will run: hs2 perf cores"; exit 0 ;;
  "config lock") echo locked; exit 0 ;;
esac
exit 0
B
chmod +x "$T/hs2"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh"
BIN="$T/hs2"; export T OLDBIN
MENU_CONF="$T/menu.conf"; UNIT_DIR="$T/units"; mkdir -p "$UNIT_DIR"
printf 'lang=%s\nmode=simple\ncolor=off\n' "${MLANG:-en}" > "$MENU_CONF"
NM_DOC_FIXES_FILE="$T/fixes.json"
hr(){ :; }
tm_units(){ echo hs2-a; echo hs2-b; }; tm_cfg(){ echo "$T/$1.json"; }; unit_desc(){ echo "test"; }
systemctl(){ echo "systemctl $*" >> "$T/calls"; case "$*" in *"is-enabled hs2-alert-hs2-a.timer"*|daemon-reload) return 0 ;; esac; return 1; }
[ "${NONROOT:-0}" = 1 ] && id(){ echo 1000; }
server_doctor(){ echo "CLASSIC-DOCTOR" >> "$T/calls"; }
tm_alerts(){ echo "CLASSIC-ALERTS $1" >> "$T/calls"; }
nm_main
echo "MENU_DONE"
INNER
run(){ rm -f "$T/calls"; printf "$1" | script -qec "T='$T' OLDBIN=${OLDBIN:-0} NONROOT=${NONROOT:-0} MLANG=${MLANG:-en} LANG=en_US.UTF-8 LC_ALL= LC_CTYPE= bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'; }
locked(){ awk -v c="$1" '/^config lock/{l=1} /^config unlock/{l=0} index($0,c)==1{print (l?"IN":"OUT")}' "$T/calls" | grep -qx IN; }
clean(){ ! echo "$out" | grep -Eq "stopped unexpectedly|unbound variable|line [0-9]+:"; }

# ---- screen 6 -------------------------------------------------------------
out=$(run '6\n0\n0\n')
check "6: settings via hs2 alert conf show, per-tunnel timer state" 'grep -q "^alert conf show --lang en" "$T/calls" && echo "$out" | grep -q "hs2-a: ON" && echo "$out" | grep -q "hs2-b: OFF" && echo "$out" | grep -q "quiet hours"'
check "6: menu exits cleanly" 'clean'
out=$(run "6\n1\n$TOKEN\n\n0\n0\n")
check "6: bot token read hidden and passed on stdin" '[ "$(cat "$T/token")" = "$TOKEN" ] && grep -q "^alert conf token --lang en" "$T/calls"'
check "6: the token is never in argv or printed (only the pty echo of the typed input, before the prompt)" '! grep -q "SecretToken" "$T/calls" && ! echo "$out" | sed -n "/Bot token from/,\$p" | grep -q "SecretToken"'
out=$(run '6\n2\n-100123\n\n3\nsocks5://127.0.0.1:1080\n\n4\nfa\n\n0\n0\n')
check "6: chat id, proxy (after a reachability check), language" 'grep -q "^alert conf set chat_id=-100123" "$T/calls" && grep -q "^alert telegram-check" "$T/calls" && grep -q "^alert conf set proxy=socks5://127.0.0.1:1080" "$T/calls" && grep -q "^alert conf set lang=fa" "$T/calls"'
check "6: Iran server without Telegram: the proxy hint is shown" 'echo "$out" | grep -q "NOT reachable from this server (typical on Iran servers): set a proxy"'
out=$(run '6\n1\n\n\n2\n\n3\n\n\n0\n0\n')
check "6: Enter keeps token, chat id and proxy (nothing written)" '! grep -q "^alert conf token" "$T/calls" && ! grep -q "^alert conf set" "$T/calls"'
out=$(run '6\n1\n-\n\n2\n-\n\n3\n-\n\n0\n0\n')
check "6: - removes token, chat id and proxy" '[ "$(cat "$T/token")" = "" ] && grep -q "^alert conf set chat_id= " "$T/calls" && grep -q "^alert conf set proxy= " "$T/calls"'
out=$(run '6\n7\n8\n7\n9\n7\n42\n0\n0\n')
check "6: alert kind toggles (rtt OFF → on, psi on → off), unknown number refused" 'grep -q "^alert rule rtt on" "$T/calls" && grep -q "^alert rule psi off" "$T/calls" && echo "$out" | grep -q "No such alert kind"'
out=$(run '6\n8\ncpu_pressure_pct=40\n\n9\n23:30-07:00\n\ns\n21:00\nusers,cuts\n\ns\n\nall\n\n0\n0\n')
check "6: threshold, quiet hours across midnight, summary time + content" 'grep -q "^alert conf set cpu_pressure_pct=40" "$T/calls" && grep -q "^alert conf set quiet_hours=23:30-07:00" "$T/calls" && grep -q "^alert conf set summary_time=21:00" "$T/calls" && grep -q "^alert conf set summary_items=users,cuts" "$T/calls" && grep -q "^alert conf set summary_items= " "$T/calls"'
out=$(run '6\nh\n\nn\n\n0\n0\n')
check "6: 7-day history and summary now" 'grep -q "^alert history --days 7 --lang en" "$T/calls" && grep -q "^alert summary-now --lang en" "$T/calls" && echo "$out" | grep -q "summary sent for 2"'
out=$(run '6\n6\n2\n\n0\n0\n')
check "6: alerts ON for one tunnel writes its timer" '[ -f "$T/units/hs2-alert-hs2-b.timer" ] && grep -q "alert -c $T/hs2-b.json -u hs2-b" "$T/units/hs2-alert-hs2-b.service"'
out=$(run '6\n5\n1\n\n0\n0\n')
check "6: test message for the picked tunnel" 'grep -q "^alert -c $T/hs2-a.json -u hs2-a -test" "$T/calls"'
out=$(NONROOT=1 run '6\n6\n1\n\n0\n0\n')
check "6: not root: explained, no timer written" 'echo "$out" | grep -q "Not root" && echo "$out" | grep -q "Needs root" && [ ! -f "$T/units/hs2-alert-hs2-a.timer" ]'
out=$(MLANG=fa run '6\n0\n0\n')
check "6: Persian screen" 'echo "$out" | grep -q "پایش و هشدارها" && grep -q "^alert conf show --lang fa" "$T/calls"'
out=$(OLDBIN=1 run '6\n1\n0\n')
check "6: old binary: the classic alert screen" 'grep -q "^CLASSIC-ALERTS" "$T/calls"'

# ---- screen 7 -------------------------------------------------------------
rm -f "$T/fixes.json"
out=$(run '7\n0\n0\n')
check "7: no fix item before a run" '! echo "$out" | grep -q "Apply a fix"'
out=$(run '7\n1\n\n3\n\n0\n0\n')
check "7: run all, and one category (cpu)" 'grep -q "^doctor --deep --fixes --lang en" "$T/calls" && grep -q "^doctor --deep --fixes --only cpu --lang en" "$T/calls"'
check "7: numbered fixes shown, fix item appears with the count" 'echo "$out" | grep -q "fix: 2) park 2.2.2.2" && echo "$out" | grep -q "Apply a fix (number from the report; preview first) \[3\]"'
out=$(run '7\n1\n\nf\n1\n\n0\n0\n')
check "7: fix 1 through hs2 doctor --fix under the admin lock" 'locked "doctor --fix 1 --hours 6 --lang en"'
out=$(run '7\n1\n\nf\n2\n3\n\n0\n0\n')
check "7: park fix asks for the hours" 'locked "doctor --fix 2 --hours 3 --lang en"'
out=$(run '7\n1\n\nf\n3\n0\n0\n0\n')
check "7: alerts-off fix opens screen 6" 'grep -q "^alert conf show" "$T/calls" && ! grep -q "^doctor --fix 3" "$T/calls"'
out=$(run '7\n2\n\n5\n\n6\n\n0\n0\n')
check "7: network, reality, panel categories" 'grep -q -- "--only network" "$T/calls" && grep -q -- "--only reality" "$T/calls" && grep -q -- "--only panel" "$T/calls"'
out=$(NONROOT=1 HOME="$T" run '7\nr\n\n0\n0\n')
check "7: report file saved (non-root: in HOME)" 'ls "$T"/hs2-doctor-*.txt >/dev/null 2>&1 && echo "$out" | grep -q "Report saved"'
check "7: menu exits cleanly" 'clean'
out=$(OLDBIN=1 run '7\n\n0\n')
check "7: old binary: the classic read-only doctor" 'grep -q "^CLASSIC-DOCTOR" "$T/calls"'

[ $fail = 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }

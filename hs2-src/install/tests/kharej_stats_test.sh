#!/bin/bash
# The Kharej side's users and traffic in the installer's live views:
#   - a status file from this hs2 ("counted": true) shows the Kharej server's
#     own open connections, active ones, Mbit/s and the last minute's peak;
#   - a status file from an older hs2 (no "counted") on a Kharej server says
#     the users are counted on the Iran server instead of showing 0;
#   - the Iran side is unchanged.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-400)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP kharej_stats_test (no script(1))"; exit 0; }
awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"

printf '{"mode":"listen","carrier":"mtcp","reverse":true,"addr":"1.2.3.4:2082"}\n' > "$T/kh.json"
printf '{"mode":"dial","carrier":"mtcp","reverse":true,"addr":"0.0.0.0:2082"}\n' > "$T/ir.json"
newkh='{"links":64,"target":64,"min":2,"max":300,"users":5892,"mbit":183.2,"peak_mbit":201.5,"flowing":512,"counted":true,"phase":"following"}'
oldkh='{"links":64,"target":64,"min":2,"max":64,"users":0,"mbit":0,"phase":"following"}'
iran='{"links":64,"target":64,"min":2,"max":300,"users":5880,"mbit":183.0,"flowing":512,"serving":64,"counted":true,"phase":"steady"}'

cat > "$T/stub.sh" <<STUB
source "$T/core.sh" >/dev/null 2>&1
status_path(){ echo "$T/st.json"; }; status_fresh(){ return 0; }
tm_state(){ echo running; }; tm_state_label(){ echo running; }; tm_uptime(){ echo 1m; }
tm_role(){ echo role; }; tm_dir(){ echo reverse; }; tm_transport(){ echo mtcp; }; tm_endpoint(){ echo ep; }
tm_peers(){ :; }; tm_health_lines(){ :; }; hr(){ :; }; info(){ :; }
STUB

pattern(){ # cfg status-json
  printf '%s\n' "$2" > "$T/st.json"
  bash -c "source '$T/stub.sh'; tm_pattern '$1'" 2>&1
}
monitor(){ # cfg status-json — one refresh, then Enter
  printf '%s\n' "$2" > "$T/st.json"
  (sleep 1; printf '\n') | timeout 20 script -qec "bash -c \"source '$T/stub.sh'; tm_monitor u '$1'\"" /dev/null 2>&1 | tr -d '\r'
}

out=$(pattern "$T/kh.json" "$newkh")
check "pattern, Kharej (new): its own connections and active" 'echo "$out" | grep -q "5892 connections, 512 active" && echo "$out" | grep -q "183.2 Mbit/s"'
out=$(pattern "$T/kh.json" "$oldkh")
check "pattern, Kharej (older hs2): counted on the Iran server, no 0" 'echo "$out" | grep -q "users counted on the Iran server" && ! echo "$out" | grep -q " 0 connections"'
out=$(pattern "$T/ir.json" "$iran")
check "pattern, Iran: unchanged" 'echo "$out" | grep -q "5880 connections, 512 active"'

out=$(monitor "$T/kh.json" "$newkh")
check "monitor, Kharej (new): users line" 'echo "$out" | grep -q "Users:   5892 open connections, 512 active"'
check "monitor, Kharej (new): speed and peak" 'echo "$out" | grep -q "Speed:   183.2 Mbit/s · peak 201.5 Mbit/s in the last minute"'
out=$(monitor "$T/kh.json" "$oldkh")
check "monitor, Kharej (older hs2): counted on the Iran server" 'echo "$out" | grep -q "Users:   counted on the Iran server"'
check "monitor, Kharej (older hs2): no 0 users / 0 speed" '! echo "$out" | grep -q "Users:   0" && ! echo "$out" | grep -q "Speed:   0"'
out=$(monitor "$T/ir.json" "$iran")
check "monitor, Iran: users line" 'echo "$out" | grep -q "Users:   5880 open connections, 512 active"'
[ "$fail" = 0 ] && echo "OK: kharej stats views" || exit 1

#!/bin/bash
# udp_ports_test.sh: with "Also forward UDP on the user ports? y", the Iran
# setup checked the user ports with port_free (TCP) only. A port whose UDP
# another program held passed, and the hs2 of that time then exited at every
# start ("listen udp :PORT: bind: address already in use"): the owner's
# "answered y and the tunnel did not come up" (R1 round 3, r1-udp). Now
# udp_ports_note names such ports at setup (hs2 itself serves their TCP and
# retries their UDP). Tested here:
#   - a port held on UDP is named, a free one is not, and it never fails
#     (the installer runs under set -e);
#   - all four Iran branches that write "udp": $UDP call it (direct tcp and
#     tun/tcp with the link's answer, reverse tcp and tun/tcp after the
#     question).
# Runs inside sandbox.sh like the other installer tests (its own PID and
# mount namespaces); it only binds one UDP port on 127.0.0.1 of its own.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
case "${HS2_SANDBOX:-}" in
  proven|nonroot) ;;
  *) exec bash "$HERE/sandbox.sh" bash "$0" "$@" ;;
esac
T=$(mktemp -d); trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

sed -n '/^udp_port_free(){/p;/^udp_ports_note(){/,/^}/p' "$INST" > "$T/fns.sh"
for f in udp_port_free udp_ports_note; do
  grep -q "^$f(){" "$T/fns.sh" || { echo "FAIL $f not found in install.sh"; exit 1; }
done

# a UDP port another program holds, and a free one next to it
python3 -c '
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("127.0.0.1", 0))
print(s.getsockname()[1], flush=True); time.sleep(120)' > "$T/held" &
for i in $(seq 1 50); do [ -s "$T/held" ] && break; sleep 0.1; done
HELD=$(head -1 "$T/held")
FREE=$(python3 -c 'import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
[ -n "$HELD" ] && [ -n "$FREE" ] || { echo "FAIL no test ports"; exit 1; }

note(){ bash -c "set -euo pipefail; warn(){ echo \"WARN: \$*\"; }; source '$T/fns.sh'; udp_ports_note '$1'; echo RC=\$?" 2>&1; }
out=$(note "$FREE")
check "a free UDP port is not named" '! echo "$out" | grep -q WARN && echo "$out" | grep -qx RC=0'
out=$(note "$HELD")
check "a UDP port another program holds is named" 'echo "$out" | grep -q "WARN: UDP on user port(s) $HELD is already taken" && echo "$out" | grep -qx RC=0'
out=$(note "$FREE,$HELD")
check "only the held one of two is named, and setup goes on (set -e)" 'echo "$out" | grep -q "port(s) $HELD is" && ! echo "$out" | grep -q "$FREE" && echo "$out" | grep -qx RC=0'

n=$(grep -c 'if \[ "\$UDP" = true \]; then udp_ports_note "\$PORTS"; fi' "$INST")
check "all four Iran branches that write \"udp\" check the user ports' UDP (found $n)" '[ "$n" = 4 ]'

exit $fail

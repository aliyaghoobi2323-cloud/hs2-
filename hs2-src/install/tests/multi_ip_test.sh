#!/bin/bash
# Regression for the SIGPIPE in first_public_ip on a multi-IP server. local_ips
# must list every IP, and first_public_ip must return the first WITHOUT the
# script dying — the field hit "stopped unexpectedly (status 141) at:
# defip=$(first_public_ip)" on a 6-IP Iran server. A lab with 1-2 IPs missed it.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }

# a fake `ip` printing 6 IPv4 addresses the way `ip -4 -o addr show` does
mkdir -p "$T/bin"
cat > "$T/bin/ip" <<'IP'
#!/bin/bash
cat <<'OUT'
1: lo    inet 127.0.0.1/8 scope host lo
2: eth0    inet 5.57.38.163/24 scope global eth0
2: eth0    inet 81.12.35.145/24 scope global eth0
2: eth0    inet 85.133.250.174/24 scope global eth0
3: eth1    inet 206.1.97.84/24 scope global eth1
3: eth1    inet 153.52.92.119/24 scope global eth1
4: hs0    inet 10.77.0.1/30 scope global hs0
OUT
IP
chmod +x "$T/bin/ip"

sed -n '/^local_ips(){/,/^}/p;/^first_public_ip(){/p;/^show_ips(){/p;/^cfg_tun_iface(){/,/^}/p' "$INST" > "$T/fns.sh"
cat > "$T/run.sh" <<RUN
set -euo pipefail
export PATH="$T/bin:\$PATH"
tm_units(){ echo hs0dummy; }           # no real units; cfg_tun_iface sees nothing
tm_cfg(){ echo /nonexistent; }
jget(){ :; }                            # cfg_tun_iface -> empty carrier/iface
source "$T/fns.sh"
# the exact call the installer aborted on, under the same set -euo pipefail
defip=\$(first_public_ip)
echo "FIRST=\$defip"
echo "COUNT=\$(local_ips | wc -l)"
echo "ALL=\$(local_ips | tr '\n' ',')"
RUN
out=$(bash "$T/run.sh" 2>&1); rc=$?
check "first_public_ip succeeds on a 6-IP server (no SIGPIPE abort)" '[ $rc = 0 ]'
check "first_public_ip returns the first address" 'echo "$out" | grep -qx "FIRST=5.57.38.163"'
check "local_ips lists all six public IPs" 'echo "$out" | grep -qx "COUNT=6"'
check "local_ips excludes lo" '! echo "$out" | grep -q 127.0.0.1'
[ $rc = 0 ] || echo "  output: $out"
exit $fail

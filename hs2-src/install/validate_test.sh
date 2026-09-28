#!/usr/bin/env bash
# Tests for the installer's input validation.   bash install/validate_test.sh
# Sources install.sh with HS2_LIB=1 (helpers only, no menu). Needs root, like
# the installer itself, and iproute2 for the local-address checks.
here=$(cd "$(dirname "$0")" && pwd)
HS2_LIB=1 source "$here/install.sh" 2>/dev/null
set +e +u
fail=0; n=0
yes(){ n=$((n+1)); "$@" 2>/dev/null || { echo "FAIL (should accept): $*"; fail=1; }; }
no(){  n=$((n+1)); ! "$@" 2>/dev/null || { echo "FAIL (should reject): $*"; fail=1; }; }

# ports
yes is_port 1; yes is_port 443; yes is_port 65535
no is_port 0; no is_port 65536; no is_port 0443; no is_port -1; no is_port abc; no is_port ""
no is_port "8443 "; no is_port 99999999999999999999

# IPv4
yes is_ipv4 203.0.113.5; yes is_ipv4 0.0.0.0; yes is_ipv4 255.255.255.255; yes is_ipv4 10.0.0.1
no is_ipv4 256.1.1.1; no is_ipv4 1.2.3; no is_ipv4 1.2.3.4.5; no is_ipv4 01.2.3.4
no is_ipv4 1.2.3.a; no is_ipv4 " 1.2.3.4"; no is_ipv4 ""; no is_ipv4 '1.2.3.4"'

# IPv6
yes is_ipv6 ::1; yes is_ipv6 ::; yes is_ipv6 2001:db8::1; yes is_ipv6 fe80::1:2:3:4
yes is_ipv6 2001:0db8:0000:0000:0000:ff00:0042:8329
no is_ipv6 2001:db8::1::2; no is_ipv6 12345::1; no is_ipv6 2001:db8:::1; no is_ipv6 g::1
no is_ipv6 1:2:3:4:5:6:7; no is_ipv6 1:2:3:4:5:6:7:8:9; no is_ipv6 203.0.113.5; no is_ipv6 ""

# domains
yes is_domain vpn.example.com; yes is_domain a-b.example.co; yes is_domain x.io
no is_domain example; no is_domain -bad.example.com; no is_domain bad-.example.com
no is_domain 'vpn.example.com"'; no is_domain 'a b.com'; no is_domain 203.0.113.5; no is_domain ""
no is_domain "$(printf 'a%.0s' {1..64}).com"

# host:port
yes is_hostport 127.0.0.1:8443; yes is_hostport localhost:8443; yes is_hostport '[::1]:8443'
yes is_hostport panel.example.com:443
no is_hostport 8443; no is_hostport 127.0.0.1; no is_hostport 127.0.0.1:0; no is_hostport ::1:8443
no is_hostport '[1.2.3.4]:80'; no is_hostport 127.0.0.1:8443:1; no is_hostport 'a"b:80'

# keys
yes is_key "$(printf 'ab%.0s' {1..32})"
no is_key "$(printf 'ab%.0s' {1..31})a"; no is_key "$(printf 'AB%.0s' {1..32})"; no is_key xyz

# hostport joins IPv6 with brackets
[ "$(hostport 203.0.113.5 2096)" = 203.0.113.5:2096 ] || { echo "FAIL hostport v4"; fail=1; }
[ "$(hostport 2001:db8::1 2096)" = '[2001:db8::1]:2096' ] || { echo "FAIL hostport v6"; fail=1; }
n=$((n+2))

# local addresses: loopback is local, a documentation address is not
yes is_local_ip 127.0.0.1; no is_local_ip 198.51.100.77
yes opt_local_ip ""; yes opt_local_ip 127.0.0.1; no opt_local_ip 198.51.100.77; no opt_local_ip nope

# ports list (uses ss); pick ports unlikely to be in use
UDP=false
yes ports_ok 45123; yes ports_ok 45123,45124
no ports_ok ""; no ports_ok 45123,45123; no ports_ok 45123,abc; no ports_ok 0
# a port that is in use is rejected
python3 -c 'import socket,time;s=socket.socket();s.bind(("127.0.0.1",45125));s.listen();time.sleep(3)' &
sleep 0.5; no ports_ok 45125; wait

[ $fail = 0 ] && echo "ok: $n validation checks passed" || { echo "validation tests FAILED"; exit 1; }

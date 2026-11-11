#!/bin/bash
# sec41 probe battery: direct target vs reality listener (unauthenticated)
T=www.microsoft.com; L=5.160.110.234:18743; D=$(getent ahostsv4 $T | head -1 | awk '{print $1}'):443
run() { # name cmd(addr)
  for a in $D $L; do
    out=$(eval "$2" 2>&1 | sed -E 's/[0-9a-f]{40,}//g; s/(Session-ID|Master-Key|TLS session ticket|Start Time|Timeout|Session-ID-ctx|[0-9]{4} - ).*//' | grep -E "$3" | sort | uniq | md5sum | cut -c1-8)
    echo "$1 $a $out"
  done
}
run tls13      'echo | timeout 8 openssl s_client -connect $a -servername $T -tls1_3 2>&1' 'Protocol|Cipher|Verify return|Server public key|depth|ALPN|alert'
run tls12only  'echo | timeout 8 openssl s_client -connect $a -servername $T -tls1_2 2>&1' 'Protocol|Cipher|Verify return|alert|error'
run tls10only  'echo | timeout 8 openssl s_client -connect $a -servername $T -tls1 2>&1' 'Protocol|Cipher|alert|error|errno'
run nosni      'echo | timeout 8 openssl s_client -connect $a -noservername 2>&1' 'Protocol|Cipher|depth|alert|subject'
run othersni   'echo | timeout 8 openssl s_client -connect $a -servername example.com 2>&1' 'Protocol|Cipher|depth|alert|subject'
run alpnh2     'echo | timeout 8 openssl s_client -connect $a -servername $T -alpn h2,http/1.1 2>&1' 'ALPN|Protocol'
run curlh2     'curl -s -o /dev/null -m 10 --http2 --resolve $T:443:${a%:*} --connect-to $T:443:$a https://$T/ -w "%{http_code} %{http_version} %{size_download}\n" | cut -c1-9' '.'
run plainhttp  'printf "GET / HTTP/1.1\r\nHost: $T\r\n\r\n" | timeout 8 nc ${a%:*} ${a#*:} | head -1' '.'
run garbage    'head -c 64 /dev/urandom | timeout 8 nc ${a%:*} ${a#*:} | head -c 7 | xxd -p' '.'

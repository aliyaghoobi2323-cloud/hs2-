#!/bin/bash
# rel50: the classic installer writes the tunnel's tls name ("sni") on BOTH
# sides, like the new menu: every config heredoc of a listening side that has
# a certificate (cert_file) also writes "sni". Without it a direct listener
# had no tls name, and a "both" front sent every non-reality hello to the
# reality target (rel48 §11.7.1; hs2 now falls back to the certificate's
# names, the installer no longer relies on that).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
# one line per heredoc config: "<mode>|<has cert>|<has sni>"
blocks=$(awk '/write_cfg_checked <<EOF/{b=1;t="";next} b&&/^EOF$/{b=0;
  m=(t ~ /"mode": "listen"/)?"listen":"dial"; c=(t ~ /"cert_file"/)?1:0; s=(t ~ /"sni": "\$DOMAIN"/)?1:0; print m"|"c"|"s; next} b{t=t $0}' "$INST")
nl=$(echo "$blocks" | grep -c '^listen|1|')
nls=$(echo "$blocks" | grep -c '^listen|1|1$')
check "every listener heredoc with a certificate writes sni ($nls of $nl)" '[ "$nl" -ge 2 ] && [ "$nl" = "$nls" ]'
nd=$(echo "$blocks" | grep -c '^dial|')
nds=$(echo "$blocks" | grep -c '^dial|.|1$')
check "dialer heredocs still write sni ($nds of $nd)" '[ "$nd" -ge 2 ] && [ "$nds" -ge 2 ]'
# the direct listener's sni is the domain its certificate was issued for
check "direct listener sni = the cert domain" 'grep -q "\"addr\": \"\$BINDADDR:\$TPORT\", \"sni\": \"\$DOMAIN\"," "$INST"'
exit $fail

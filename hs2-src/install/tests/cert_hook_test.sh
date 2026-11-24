#!/bin/bash
# b430: the certbot renew/deploy hook. It was `pkill -HUP -x hs2`, which matched no process once the binary had a
# versioned name (comm = basename, e.g. hs2-t1.r69r7): renewed certificates loaded only at the next restart. Now
# install_cert_hook writes a helper that finds the hs2 tunnel units at run time by unit name and reloads them.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"; ZD="$HERE/../zdu"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
export HS2_CERT_HOOK="$T/sbin/hs2-reload-certs"
sed -n '/^CERT_HOOK=/p;/^install_cert_hook(){/,/^}/p' "$INST" > "$T/fns.sh"
check "install.sh: CERT_HOOK is the helper (overridable for tests), not a process-name signal" 'grep -qx "CERT_HOOK=\"\${HS2_CERT_HOOK:-/usr/local/sbin/hs2-reload-certs}\"" "$INST"'
bash -c "set -euo pipefail; source '$T/fns.sh'; install_cert_hook; install_cert_hook"
check "install_cert_hook: writes an executable helper (idempotent, no temp left)" '[ -x "$HS2_CERT_HOOK" ] && [ ! -e "$HS2_CERT_HOOK.tmp" ] && bash -n "$HS2_CERT_HOOK"'

# fake systemctl: units with their ExecStart / ExecReload
mkdir -p "$T/bin"
cat > "$T/units" <<'U'
hs2.service|/usr/local/bin/hs2-t1.r69r7 run -c /etc/hs2/config.json|/bin/kill -HUP $MAINPID
hs2-out.service|/usr/local/bin/hs2-t1.r69r7 run -c /etc/hs2/out.json|/bin/kill -HUP $MAINPID
hs2-t3.service|/usr/local/bin/hs2 run -c /etc/hs2/t3.json|
hs2-alert-hs2.service|/usr/local/bin/hs2 alert -c /etc/hs2/config.json|
hs2-stats.service|/usr/local/sbin/hs2-stats.sh|
xhs2.service|/usr/local/bin/hs2 run -c /etc/hs2/x.json|/bin/kill -HUP $MAINPID
nginx.service|/usr/sbin/nginx|/usr/sbin/nginx -s reload
U
cat > "$T/bin/fsc" <<S
#!/bin/bash
echo "\$*" >> "$T/calls"
f(){ awk -F'|' -v u="\$1" -v c="\$2" '\$1==u{print \$c}' "$T/units"; }
case "\$1" in
  list-units) awk -F'|' '{printf "%s loaded active running x\n", \$1}' "$T/units";;
  show) [ "\$3" = ExecStart ] && { s=\$(f "\$5" 2); [ -n "\$s" ] && echo "{ path=x ; argv[]=\$s ; }"; }
        [ "\$3" = ExecReload ] && { s=\$(f "\$5" 3); [ -n "\$s" ] && echo "{ path=x ; argv[]=\$s ; }"; } ;;
esac
exit 0
S
chmod 0755 "$T/bin/fsc"
export HS2_SYSTEMCTL="$T/bin/fsc"
"$HS2_CERT_HOOK"; rc=$?
check "hook: exit 0" '[ "$rc" = 0 ]'
check "hook: reloads hs2 and hs2-out by unit name (the prod t1 pair)" 'grep -qx "reload hs2.service" "$T/calls" && grep -qx "reload hs2-out.service" "$T/calls"'
check "hook: a tunnel unit without ExecReload gets SIGHUP to its main process via systemd" 'grep -qx "kill -s HUP --kill-whom=main hs2-t3.service" "$T/calls"'
check "hook: helper units (alert, stats) and non-hs2 units (xhs2, nginx) untouched" '! grep -qE "^(reload|kill).*(alert|stats|xhs2|nginx)" "$T/calls"'
check "hook: exactly 3 actions" '[ "$(grep -cE "^(reload|kill) " "$T/calls")" = 3 ]'
# a tunnel added after the hook was written is found at run time
echo 'hs2-new.service|/usr/local/bin/hs2-t2.r70 run -c /etc/hs2/new.json|/bin/kill -HUP $MAINPID' >> "$T/units"; : > "$T/calls"
"$HS2_CERT_HOOK"
check "hook: a tunnel added later is reloaded too (no fixed list)" 'grep -qx "reload hs2-new.service" "$T/calls" && [ "$(grep -cE "^(reload|kill) " "$T/calls")" = 4 ]'
# a failing reload is reported in the exit status (certbot logs it)
printf '#!/bin/bash\n[ "$1" = reload ] && [ "$2" = hs2-out.service ] && exit 1\nexec %s "$@"\n' "$T/bin/fsc" > "$T/bin/fsc2"; chmod 0755 "$T/bin/fsc2"
HS2_SYSTEMCTL="$T/bin/fsc2" "$HS2_CERT_HOOK"; rc=$?
check "hook: a failed reload -> exit 1, the other units still reloaded" '[ "$rc" = 1 ] && grep -qx "reload hs2-new.service" "$T/calls"'

# no process-name signalling of hs2 left in the installer or the server-side scripts (comments aside)
check "no pkill/pgrep/killall by the hs2 process name in install.sh or install/zdu" '! grep -hvE "^[[:space:]]*#" "$INST" "$ZD"/*.sh | grep -qE "(pkill|pgrep|killall)[^|;&]*[[:space:]]-?x?[[:space:]]*hs2\b"'
check "no pkill/pgrep at all in install.sh code" '! grep -hvE "^[[:space:]]*#" "$INST" | grep -qE "\b(pkill|pgrep|killall)\b"'
exit $fail

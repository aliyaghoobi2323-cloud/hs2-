#!/bin/bash
# The link-pool screen (Tuning → 6) must say which server's values actually
# count, matching how the engine sizes the pool (stream pool and datagram tun
# pool alike): the Iran side always decides; in direct the Kharej side only
# accepts (its values do nothing); in reverse the Kharej side dials and caps the
# count at its own min/max; 'tls' is always one link; single-session transports
# have no pool. Where the values cannot have any effect, the screen must not
# write them. Driven on a pty under the installer's real `set -euo pipefail`.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "      got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-300)"; fail=1; fi; }
command -v script >/dev/null 2>&1 || { echo "SKIP tm_tune_links_test (no script(1))"; exit 0; }

awk '/^case "\$\{1:-\}" in$/{exit} {print}' "$INST" > "$T/core.sh"
cat > "$T/inner.sh" <<'INNER'
source "$T/core.sh" >/dev/null 2>&1
[ -n "${FAKEBIN:-}" ] && BIN="$FAKEBIN"
say(){ printf '%s\n' "$*"; }; warn(){ say "$*"; }; info(){ say "$*"; }; ok(){ say "$*"; }; hr(){ :; }
tm_cfgset(){ echo "CFGSET $2=$3"; }
tm_apply_restart(){ echo "APPLIED"; }
tm_tune_links hs2-x "$CFGF"
echo "RETURNED"
INNER
run(){ # carrier mode reverse keys
  printf '{"mode":"%s","reverse":%s,"carrier":"%s","min_links":2,"max_links":32,"per_link":8}\n' "$2" "$3" "$1" > "$T/c.json"
  printf "$4" | script -qec "T='$T' CFGF='$T/c.json' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
for car in mtcp l3mtcp dgtun; do
  out=$(run "$car" dial false '\n40\n\n')
  check "$car, Iran direct: its values count; new max written" 'echo "$out" | grep -q "these are the values that count" && echo "$out" | grep -q "CFGSET max_links=40" && echo "$out" | grep -q APPLIED'
  out=$(run "$car" dial true '\n40\n\n')
  check "$car, Iran reverse: told Kharej caps at its own max; values written" 'echo "$out" | grep -q "caps the count at ITS OWN min/max" && echo "$out" | grep -q "CFGSET max_links=40"'
  out=$(run "$car" listen true '\n40\n\n')
  check "$car, Kharej reverse: told it only enforces limits; values written" 'echo "$out" | grep -q "only" && echo "$out" | grep -q "enforces its min/max as limits" && echo "$out" | grep -q "CFGSET max_links=40"'
  out=$(run "$car" listen false '\n')
  check "$car, Kharej direct: NO effect here, nothing written" 'echo "$out" | grep -q "NO" && echo "$out" | grep -q "Change them on the Iran server" && ! echo "$out" | grep -q CFGSET && echo "$out" | grep -q RETURNED'
done
out=$(run tls dial false '\n')
check "tls: always ONE link, nothing written" 'echo "$out" | grep -q "exactly ONE link" && ! echo "$out" | grep -q CFGSET && echo "$out" | grep -q RETURNED'
for car in udp auto noise reality; do
  out=$(run "$car" dial false '\n')
  check "$car: single session, nothing written" 'echo "$out" | grep -q "single session" && ! echo "$out" | grep -q CFGSET'
done
# tun over icmp: the screen names the icmp ceiling (8), not the hardware's.
cat > "$T/hs2" <<'FAKE'
#!/bin/bash
[ "${1:-}" = recommend-links ] || exit 2
icmp=0; for a in "$@"; do [ -f "$a" ] && grep -q '"encap":"icmp"' "$a" && icmp=1; done
case "$*" in
  *--why*) [ $icmp = 1 ] && echo "8  (tun over icmp: one echo id per carrier between the same two IPs, so more add no bandwidth; the hardware alone would allow 300: 16 GB)" || echo "300  (16 GB RAM, 20 cores)" ;;
  *) [ $icmp = 1 ] && echo 8 || echo 300 ;;
esac
FAKE
chmod +x "$T/hs2"
runi(){ # max keys
  printf '{"mode":"dial","reverse":true,"carrier":"dgtun","encap":"icmp","min_links":2,"max_links":%s,"per_link":8}\n' "$1" > "$T/c.json"
  printf "$2" | script -qec "T='$T' FAKEBIN='$T/hs2' CFGF='$T/c.json' bash '$T/inner.sh'" /dev/null 2>&1 | tr -d '\r'
}
out=$(runi 0 '\n\n\n')
check "icmp auto: names the icmp ceiling, no 300-carrier note" 'echo "$out" | grep -q "Auto ceiling for this tunnel: max 8  (tun over icmp" && echo "$out" | grep -q "auto, now 8" && ! echo "$out" | grep -q "up to 300"'
out=$(runi 40 '\n\n\n')
check "icmp fixed 40: told auto gives 8 over icmp" 'echo "$out" | grep -q "fixed at 40; auto gives 8 over icmp"'
exit "$fail"

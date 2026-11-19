#!/bin/bash
# sec370 (sec353 1g): install.sh fix_perms tightens hs2's own files left by
# older releases (dirs 0700, files owner-only, owner bits kept), never follows
# a symlink, leaves the ACME webroot alone, and HS2_KEEP_PERMS=1 /
# HS2_RUN_SHARED=1 opt out. Runs as any user in a sandbox.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); INST="$HERE/../install.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
sed -n '/^fix_perms(){/,/^}/p' "$INST" > "$T/fns.sh"
grep -q '^fix_perms(){' "$T/fns.sh" || { echo "FAIL fix_perms not found in install.sh"; exit 1; }
grep -q '^  fix_perms$' "$INST" || { echo "FAIL upgrade does not call fix_perms"; fail=1; }
mk(){
  rm -rf "$T/etc" "$T/lib" "$T/run" "$T/out"; umask 022
  mkdir -p "$T/etc/certs" "$T/lib/history" "$T/lib/acme/.well-known" "$T/run"
  echo '{}' > "$T/etc/hs2.json"; chmod 644 "$T/etc/hs2.json"
  echo k > "$T/etc/certs/k.pem"; chmod 640 "$T/etc/certs/k.pem"
  echo t > "$T/etc/alert.conf"; chmod 600 "$T/etc/alert.conf"
  echo h > "$T/lib/history/1.json"; chmod 644 "$T/lib/history/1.json"
  echo c > "$T/lib/acme/.well-known/tok"; chmod 644 "$T/lib/acme/.well-known/tok"
  echo s > "$T/run/hs2.json"; chmod 644 "$T/run/hs2.json"
  echo s > "$T/lib/stats-hs2.json"; chmod 644 "$T/lib/stats-hs2.json"
  printf '#!/bin/sh\n' > "$T/lib/tool"; chmod 755 "$T/lib/tool"
  echo o > "$T/out"; chmod 644 "$T/out"; ln -s "$T/out" "$T/etc/link"
  mkdir -p "$T/outdir"; chmod 755 "$T/outdir"; echo o > "$T/outdir/f"; chmod 644 "$T/outdir/f"; ln -s "$T/outdir" "$T/etc/dlink"
}
m(){ stat -c %a "$1"; }
runfp(){ ( PERM_DIRS="$T/etc $T/lib $T/run" VARLIB_DIR="$T/lib" ACME_WEBROOT="$T/lib/acme" HS2_KEEP_PERMS="${KP:-}" HS2_RUN_SHARED="${RS:-}" RUN_SHARED_MARK="${RSM:-$T/nomark}"; source "$T/fns.sh"; fix_perms ); }

mk; runfp
check "dirs 0700" '[ "$(m "$T/etc")" = 700 ] && [ "$(m "$T/etc/certs")" = 700 ] && [ "$(m "$T/lib/history")" = 700 ] && [ "$(m "$T/run")" = 700 ]'
check "config/key/history/status files 0600" '[ "$(m "$T/etc/hs2.json")" = 600 ] && [ "$(m "$T/etc/certs/k.pem")" = 600 ] && [ "$(m "$T/lib/history/1.json")" = 600 ] && [ "$(m "$T/run/hs2.json")" = 600 ]'
check "owner exec bit kept" '[ "$(m "$T/lib/tool")" = 700 ]'
check "symlink target untouched" '[ "$(m "$T/out")" = 644 ]'
check "symlinked dir and its files untouched (b376)" '[ "$(m "$T/outdir")" = 755 ] && [ "$(m "$T/outdir/f")" = 644 ]'
check "ACME webroot left alone" '[ "$(m "$T/lib/acme/.well-known/tok")" = 644 ] && [ "$(m "$T/lib/acme")" = 755 ]'
mk; KP=1 runfp
check "HS2_KEEP_PERMS=1 changes nothing" '[ "$(m "$T/etc/hs2.json")" = 644 ] && [ "$(m "$T/etc")" = 755 ]'
# HS2_RUN_SHARED=1 only exempts the real /run/hs2 path; with the sandbox paths
# it must still fix everything (the exemption compares the literal path)
mk; RS=1 runfp
check "HS2_RUN_SHARED=1 does not exempt other dirs" '[ "$(m "$T/etc/hs2.json")" = 600 ]'
# b376: the marker a shared daemon leaves keeps its stats files 0644
mk; echo 1 > "$T/mark"; RSM="$T/mark" runfp
check "shared marker: stats-*.json left 0644, other files fixed (b376)" '[ "$(m "$T/lib/stats-hs2.json")" = 644 ] && [ "$(m "$T/lib/history/1.json")" = 600 ]'
# b378: only the var-lib dir's own stats-*.json are exempt, as in Go
mk; echo s > "$T/etc/stats-x.json"; chmod 644 "$T/etc/stats-x.json"; echo s > "$T/lib/history/stats-1.json"; chmod 644 "$T/lib/history/stats-1.json"; RSM="$T/mark" runfp
check "shared marker: stats-*.json elsewhere fixed (b378)" '[ "$(m "$T/etc/stats-x.json")" = 600 ] && [ "$(m "$T/lib/history/stats-1.json")" = 600 ] && [ "$(m "$T/lib/stats-hs2.json")" = 644 ]'
mk; RSM="$T/none" runfp
check "no marker: stats fixed" '[ "$(m "$T/lib/stats-hs2.json")" = 600 ]'
[ "$fail" = 0 ] && echo "ALL PASS"; exit "$fail"

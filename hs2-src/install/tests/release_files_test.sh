#!/bin/bash
# Release consistency, run before every push of the repository root:
#   - the root install.sh (what users curl) is byte-identical to the source copy;
#   - hs2-linux-amd64.sha256 (what install.sh checks every download against)
#     matches the committed binary. A stale hash would make every install stop
#     with "sha256 mismatch", so this must never drift.
#   - install.sh.sha256 (what install_self checks a network-fetched menu script
#     against) matches the committed install.sh — EDIT install.sh then forget to
#     regenerate this and hs2-menu silently stops installing on curl|bash hosts.
#   - gofmt -l over hs2-src is empty, EXCEPT the vendored upstream uTLS files
#     under hs2-src/third_party/utls (upstream's own formatting; only the hs2
#     additions there, hs2_*.go, are checked). Skipped when gofmt is missing.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
fail=0
ok(){   echo "PASS $1"; }
bad(){  echo "FAIL $1"; fail=1; }
sha(){ sha256sum "$1" | cut -d' ' -f1; }
hashof(){ awk 'NR==1{print $1}' "$1" 2>/dev/null; }

if cmp -s "$ROOT/install.sh" "$HERE/../install.sh"; then
  ok "root install.sh is identical to hs2-src/install/install.sh"
else
  bad "root install.sh is identical to hs2-src/install/install.sh"
fi

if [ -f "$ROOT/hs2-linux-amd64.sha256" ]; then
  ok "hs2-linux-amd64.sha256 exists"
  if [ "$(hashof "$ROOT/hs2-linux-amd64.sha256")" = "$(sha "$ROOT/hs2-linux-amd64")" ]; then
    ok "hs2-linux-amd64.sha256 matches the binary"
  else
    bad "hs2-linux-amd64.sha256 matches the binary"
  fi
else
  bad "hs2-linux-amd64.sha256 exists"
fi

if [ -f "$ROOT/install.sh.sha256" ]; then
  ok "install.sh.sha256 exists"
  if [ "$(hashof "$ROOT/install.sh.sha256")" = "$(sha "$ROOT/install.sh")" ]; then
    ok "install.sh.sha256 matches install.sh"
  else
    bad "install.sh.sha256 matches install.sh"
  fi
else
  bad "install.sh.sha256 exists"
fi

GOFMT=$(command -v gofmt 2>/dev/null || { [ -x /usr/local/go/bin/gofmt ] && echo /usr/local/go/bin/gofmt; })
if [ -n "$GOFMT" ] && [ -d "$ROOT/hs2-src" ]; then
  unformatted=$(cd "$ROOT/hs2-src" && "$GOFMT" -l . 2>&1 | grep -Ev '^third_party/utls/' ;     cd "$ROOT/hs2-src" && ls third_party/utls/hs2_*.go >/dev/null 2>&1 && "$GOFMT" -l third_party/utls/hs2_*.go 2>&1)
  if [ -z "$unformatted" ]; then
    ok "gofmt clean (vendored upstream third_party/utls excluded)"
  else
    bad "gofmt clean (vendored upstream third_party/utls excluded): $(echo $unformatted)"
  fi
else
  echo "SKIP gofmt (no gofmt on PATH)"
fi

exit $fail

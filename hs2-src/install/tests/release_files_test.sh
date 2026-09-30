#!/bin/bash
# Release consistency, run before every push of the repository root:
#   - the root install.sh (what users curl) is byte-identical to the source copy;
#   - hs2-linux-amd64.sha256 (what install.sh checks every download against)
#     matches the committed binary. A stale hash would make every install stop
#     with "sha256 mismatch", so this must never drift.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); ROOT=$(cd "$HERE/../../.." && pwd)
fail=0; check(){ if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
check "root install.sh is identical to hs2-src/install/install.sh" 'cmp -s "$ROOT/install.sh" "$HERE/../install.sh"'
check "hs2-linux-amd64.sha256 exists" '[ -f "$ROOT/hs2-linux-amd64.sha256" ]'
check "hs2-linux-amd64.sha256 matches the binary" '[ "$(awk "NR==1{print \$1}" "$ROOT/hs2-linux-amd64.sha256" 2>/dev/null)" = "$(sha256sum "$ROOT/hs2-linux-amd64" | cut -d" " -f1)" ]'
exit $fail

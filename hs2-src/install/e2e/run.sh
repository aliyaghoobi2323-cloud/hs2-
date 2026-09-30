#!/bin/bash
# End-to-end test of install.sh + tunnel manager on two systemd containers
# (iran 10.30.0.10, kharej 10.30.0.20 + a 2nd IP that appears 5 s after boot).
# The installer runs through `curl | bash` against a local stand-in for GitHub
# that serves this checkout's install.sh and a freshly built binary.
#   needs: docker (privileged), python3 + pexpect, go
#   usage: install/e2e/run.sh [phases…]   (default: all)
set -euo pipefail
E=$(cd "$(dirname "$0")" && pwd); SRC=$(cd "$E/../.." && pwd); GH=$E/.fakegh
PHASES=${*:-install manager reboot upgrade lifecycle direct adaptive}
mkdir -p "$GH"
(cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$GH/hs2-linux-amd64" ./cmd/hs2)
cp "$SRC/install/install.sh" "$GH/install.sh"
docker build -q -t hs2sysd "$E" >/dev/null
docker network create --subnet 10.30.0.0/24 --gateway 10.30.0.1 hs2net >/dev/null 2>&1 || true
(cd "$GH" && exec python3 -m http.server 8099 --bind 10.30.0.1 >/dev/null 2>&1) & GHPID=$!
trap 'kill $GHPID 2>/dev/null; docker rm -f ir kh >/dev/null 2>&1 || true' EXIT
bash "$E/fresh.sh"
cd "$E" && python3 drv.py $PHASES

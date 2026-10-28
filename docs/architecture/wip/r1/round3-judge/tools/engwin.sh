#!/bin/bash
# engwin.sh TAG: the engine package's tests alone under the rig lock, with
# a 30 min timeout (the whole package takes over go test's 10 min default;
# testwin.sh's run was cut there). Logs to /root/r3j/tests-TAG/test-engine.txt.
set -u
TAG=${1:?}
O=/root/r3j/tests-$TAG; mkdir -p "$O"
/root/r1-quiet.take.sh r1-r3j-build "judge engine tests $TAG" > /dev/null || exit 1
t0=$(date +%s)
trap "rm -f /root/r1-quiet.lock; echo engwin-$TAG \$(date -u -d @$t0 +%FT%TZ) \$(date -u +%FT%TZ) \$((\$(date +%s) - t0)) >> /root/r3j/buildtime.log" EXIT
cd /root/hs2-wt/r1-r3j/hs2-src
git log --oneline -1 > "$O/test-engine.txt"
timeout 1800 /usr/local/go/bin/go test -count=1 -timeout 28m ./engine >> "$O/test-engine.txt" 2>&1; echo "exit $?" >> "$O/test-engine.txt"

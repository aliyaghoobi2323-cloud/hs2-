#!/bin/bash
# testwin.sh NAME: the R1 red team's (second pass) test window under the shared
# rig lock (fair queue, GAP after this pass's previous block): gofmt, go vet,
# go test of every package (engine with -timeout 20m), the smux module and
# rigload, and -race on what this pass touched. Outputs to /root/rred2/NAME/
# and the worktree's redteam2/tests/NAME-*.txt.
set -u
NAME=${1:?}
R=/root/hs2-wt/r1-red2; G=/usr/local/go/bin/go
O=/root/rred2/$NAME; mkdir -p "$O"
TD=$R/docs/architecture/wip/r1/redteam2/tests
GAP=${GAP:-150}
exec 9>/root/rred2/.serial
flock 9
while last=$(cat /root/rred2/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-red2 "R1 red team 2 test window $NAME" > /dev/null || exit 1
t0=$(date +%s)
trap 'rm -f /root/r1-quiet.lock; t1=$(date +%s); echo $t1 > /root/rred2/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/rred2/locktime.log' EXIT
cd $R/hs2-src
{ echo "# $(git -C $R rev-parse --short HEAD) $(date -u +%FT%TZ)"; /usr/local/go/bin/gofmt -l . ../docs/architecture/wip/rig-scale/rigload; echo "gofmt done"
  $G vet ./... 2>&1 | tail -5; echo "vet rc=${PIPESTATUS[0]}"
  (cd third_party/smux && $G vet . 2>&1 | tail -3; echo "vet smux rc=${PIPESTATUS[0]}")
  (cd ../docs/architecture/wip/rig-scale/rigload && $G vet . 2>&1 | tail -3; echo "vet rigload rc=${PIPESTATUS[0]}"); } > "$O/vet.txt" 2>&1
$G test -count=1 $($G list ./... | grep -v /engine$) > "$O/pkgs.txt" 2>&1; echo "rc=$?" >> "$O/pkgs.txt"
(cd third_party/smux && $G test -count=1 -timeout 10m . > "$O/smux.txt" 2>&1; echo "rc=$?" >> "$O/smux.txt")
(cd ../docs/architecture/wip/rig-scale/rigload && $G test -count=1 . > "$O/rigload.txt" 2>&1; echo "rc=$?" >> "$O/rigload.txt")
$G test -count=1 -p 1 -timeout 20m ./engine > "$O/engine.txt" 2>&1; echo "rc=$?" >> "$O/engine.txt"
$G test -count=1 -race -timeout 10m -run '^TestRed2|^TestTwoWay|^TestRTHold|^TestQuietFollow|^TestOWD|^TestUDP|^TestHint' ./engine > "$O/race-engine.txt" 2>&1; echo "rc=$?" >> "$O/race-engine.txt"
(cd third_party/smux && $G test -count=1 -race -timeout 10m -run '^TestRed2|^TestGroup' . > "$O/race-smux.txt" 2>&1; echo "rc=$?" >> "$O/race-smux.txt")
for f in vet pkgs smux rigload engine race-engine race-smux; do
	grep -E "^(--- FAIL|FAIL|ok|rc=|PASS|panic|WARNING: DATA RACE)|gofmt|vet" "$O/$f.txt" | head -60 > "$TD/$NAME-$f.txt"
done
echo "done $(date -u +%T)" >> "$O/vet.txt"

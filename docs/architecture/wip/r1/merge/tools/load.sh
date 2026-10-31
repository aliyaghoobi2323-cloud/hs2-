#!/bin/bash
# load.sh PIECE COUNT: the engine package repeated under CPU load, one piece of
# the -count=5 that flaky.fa.md (R1 plan, task 19) asks for; no piece over an
# hour. Six spinning loops share the six test cores with it. Holds the test lock.
set -u
export PATH=$PATH:/usr/local/go/bin
P=$1; C=$2
S=/root/hs2-wt/r1-merge/hs2-src
H=$(git -C "$S" rev-parse --short HEAD); T=$(date -u +%Y%m%dT%H%M%SZ)
O=/root/rmerge/raw-suite/load$P-$H-$T.json
SU=/root/rmerge/suite/load$P-$H-$T.txt
/root/r1-cpu.sh timeout 3500 sh -c "for i in 1 2 3 4 5 6; do (while :; do :; done) & done; wait" > /dev/null 2>&1 &
HP=$!
cd "$S" || exit 1
t0=$(date +%s)
/root/r1-cpu.sh go test -json -count="$C" -timeout 56m ./engine > "$O" 2> "$O.err"
r=$?
kill "$HP" 2> /dev/null
{ echo "== load piece $P on $H, -count=$C next to six spinning loops: rc $r, $(( $(date +%s) - t0 )) s"; python3 /root/rmerge/tools/sumjson.py "$O" "load$P"; cat "$O.err"; } > "$SU" 2>&1
echo "load$P rc $r $(( $(date +%s) - t0 )) s $(grep "^TOTAL" "$SU")" >> /root/rmerge/load.log

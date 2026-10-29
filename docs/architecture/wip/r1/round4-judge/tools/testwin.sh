#!/bin/bash
# testwin.sh NAME WHICH: the judge's test window under the rig lock (taken
# through the fair queue, GAP after the previous block, at most 25 min),
# output to /root/r4j/tests/NAME-*.txt. WHICH: full (vet, every package but
# engine, engine with -timeout 20m) or race (-race on what the judge
# touched, smux and rigload).
set -u
NAME=${1:?}; WHICH=${2:?}
O=/root/r4j/tests; mkdir -p "$O"
GAP=${GAP:-150}
exec 9>/root/r4j/.serial
flock 9
while last=$(cat /root/r4j/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-r4j "R1 round 4 judge test window $NAME" > /dev/null || exit 1
t0=$(date +%s)
trap 'rm -f /root/r1-quiet.lock; t1=$(date +%s); echo $t1 > /root/r4j/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/r4j/locktime.log' EXIT
export PATH=$PATH:/usr/local/go/bin
cd /root/hs2-wt/r1-r4j/hs2-src || exit 1
head=$(git rev-parse --short HEAD)
case "$WHICH" in
full)
  { echo "head $head $(date -u +%FT%TZ)"; gofmt -l . ; go vet ./... && echo "vet ok"; (cd third_party/smux && go vet ./... && echo "smux vet ok"); (cd ../docs/architecture/wip/rig-scale/rigload && go vet ./... && echo "rigload vet ok"); } > "$O/$NAME-vet.txt" 2>&1
  timeout 600 go test -count=1 $(go list ./... | grep -v /engine$) > "$O/$NAME-pkgs.txt" 2>&1
  (cd third_party/smux && timeout 300 go test -count=1 ./...) > "$O/$NAME-smux.txt" 2>&1
  (cd ../docs/architecture/wip/rig-scale/rigload && timeout 200 go test -count=1 ./...) > "$O/$NAME-rigload.txt" 2>&1
  (cd ../docs/architecture/wip/rig-scale && timeout 120 python3 ana_test.py) > "$O/$NAME-ana.txt" 2>&1
  timeout 1260 go test -count=1 -timeout 20m ./engine > "$O/$NAME-engine.txt" 2>&1
  echo "engine rc $? $(date -u +%FT%TZ)" >> "$O/$NAME-engine.txt"
  ;;
race)
  timeout 1100 go test -race -count=1 -run 'Quiet|Follow|RTHold|RTShare|LatRR|RTLane|SchedRT|Sched|Budget|HostGate|HostWait|Kill|ByUser|HoldCap|Rescue|TwoWay|Place|Hint|Red|AngleTunPing' ./engine > "$O/$NAME-engine-race.txt" 2>&1
  echo "rc $? $(date -u +%FT%TZ)" >> "$O/$NAME-engine-race.txt"
  (cd third_party/smux && timeout 300 go test -race -count=1 ./...) > "$O/$NAME-smux-race.txt" 2>&1
  (cd ../docs/architecture/wip/rig-scale/rigload && timeout 200 go test -race -count=1 ./...) > "$O/$NAME-rigload-race.txt" 2>&1
  ;;
mix)
  B=/root/r4j/bins
  for c in "A-prod-edge_fin-exit 8 $B/hs2-prod $B/hs2-fin" "B-fin-edge_prod-exit 8 $B/hs2-fin $B/hs2-prod" "E-c4-edge_fin-exit 8 $B/hs2-c4f $B/hs2-fin" "F-fin-edge_c4-exit 8 $B/hs2-fin $B/hs2-c4f" "C-j3g-edge_fin-exit 1 $B/hs2-j3g $B/hs2-fin" "K-fin-edge_c4-exit 1 $B/hs2-fin $B/hs2-c4f" "L-c4-edge_fin-exit 1 $B/hs2-c4f $B/hs2-fin" "I-fin_fin 1 $B/hs2-fin $B/hs2-fin"; do
    set -- $c
    nice -n 5 /root/r4j/tools/vmmixfin.sh "$1" "$2" "$3" "$4" "" "" >> "$O/$NAME-mix.txt" 2>&1
    echo >> "$O/$NAME-mix.txt"
  done
  ;;
esac
echo "done $(date -u +%FT%TZ)" >> "$O/$NAME-$WHICH.done"

#!/bin/bash
# field.sh: three field S5 runs on the base binary (R1 baseline), run on the
# exit server. For the runs it starts an iperf3 server and an echo server on
# the exit hs0 address PEER (ports 5211 and 5212), records the exit side with
# the z5b harness (run-kharej.sh) and runs s5-iran.sh on the edge (EDGE, ssh
# over the tunnel). T seconds of load per run (120). Never port 5201.
set -u
EDGE=${EDGE:?}; PEER=${PEER:?}; T=${T:-120}; RUNS=${RUNS:-3}; PFX=${PFX:-R1B-S5}
D=$(cd "$(dirname "$0")" && pwd)
iperf3 -s -B "$PEER" -p 5211 > /tmp/r1b-iperf3-s.log 2>&1 & IP=$!
python3 "$D/echo.py" "$PEER" 5212 & EP=$!
trap "kill $IP $EP 2>/dev/null" EXIT
sleep 1
for i in $(seq 1 "$RUNS"); do
  L=$PFX-$i
  echo "$(date +%T) $L"
  /root/z5b/bin/run-kharej.sh "$L" $((T + 10)) & KP=$!
  ssh -o BatchMode=yes "$EDGE" "PEER=$PEER /root/z5b/bin/s5-iran.sh $L $T"
  wait $KP
  sleep 5
done

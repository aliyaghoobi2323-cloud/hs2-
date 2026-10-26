#!/bin/bash
# s5-iran.sh LABEL SECS: one field S5 run on the edge (the z5b harness, R1
# baseline). Production opens no test ports (forward_ports has only the panel
# port), so every flow goes through the live tunnel's hs0 to the exit's hs0
# address PEER: 2 downloads + 2 uploads (iperf3 --bidir -P 2 to PEER:5211), the
# interactive probe (32 B every 200 ms, echoed by PEER:5212) and the TUN ping
# (every 200 ms, with timestamps). Never port 5201.
set -u
L=$1 T=$2 PEER=${PEER:?}
D=/root/z5b/runs/$L
mkdir -p "$D"
start=$(date -u '+%Y-%m-%d %H:%M:%S')
echo "label=$L secs=$T start=$start $(hs2 version | grep -o 'build [0-9a-f+]*')" > "$D/meta.txt"
mpstat -P ALL 1 $((T + 6)) > "$D/mpstat.txt" 2>&1 &
pidstat -p "$(systemctl show -p MainPID --value hs2)" 1 $((T + 6)) > "$D/pidstat.txt" 2>&1 &
(end=$((SECONDS + T + 6)); while [ $SECONDS -lt $end ]; do echo "@$(date +%s.%N)"; timeout 5 hs2 status -c /etc/hs2/config.json 2>&1 | grep -E 'links:|traffic:|cpu:'; sleep 2; done) > "$D/status.txt" 2>&1 &
python3 /root/z5b/bin/probe.py "$PEER" 5212 $((T + 6)) > "$D/probe.txt" 2>&1 &
ping -D -i 0.2 -w $((T + 6)) -W 2 "$PEER" > "$D/ping.txt" 2>&1 &
sleep 3
echo "load_from=$(date +%s.%N)" >> "$D/meta.txt"
iperf3 -c "$PEER" -p 5211 --bidir -P 2 -t "$T" -i 1 -J > "$D/iperf3.json" 2> "$D/iperf3.err"
echo "load_to=$(date +%s.%N)" >> "$D/meta.txt"
wait
end=$(date -u '+%Y-%m-%d %H:%M:%S')
journalctl -u hs2 --since "$start" --until "$end" -o short-precise --no-pager > "$D/journal.txt"
echo "end=$end" >> "$D/meta.txt"

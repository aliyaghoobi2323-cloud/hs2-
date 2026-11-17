# mkrun141.py SRC DST: run.sh -> run141.sh (cs141): BG background iperf3 users ("d:150 u:40 ..."
# on ports 8451.. -> kharej 5211..), HEAVY_DELAY before the test download, HSTART file.
import sys
s = open(sys.argv[1]).read()
def rep(a, b):
    global s
    assert s.count(a) == 1, a
    s = s.replace(a, b)
rep('D=$(cd "$(dirname "$0")" && pwd)', 'D=/opt/hs2lab/rr155/labsrc/lab')
rep('''# the heavy user's own port: Iran 8444 -> kharej 127.0.0.1:5202 (iperf3)
  HPM='"port_map":"8444=127.0.0.1:5202",'; HFP=",8444"''',
    '''PMX=""; HFP=",8444"; bi=0
  for spec in ${BG:-}; do PMX="$PMX,$((8451+bi))=127.0.0.1:$((5211+bi))"; HFP="$HFP,$((8451+bi))"
    ip netns exec $KH iperf3 -s -B 127.0.0.1 -p $((5211+bi)) >/dev/null 2>&1 & echo $! >> "$W/pids"; bi=$((bi+1)); done
  HPM='"port_map":"8444=127.0.0.1:5202'$PMX'",\'''')
rep('''  ip netns exec $IR iperf3 -c $1 -p $2 -P "$HEAVY_P"''', '''  { sleep ${HEAVY_DELAY:-0}; date +%s.%N > "$W/hstart"; ip netns exec $IR iperf3 -c $1 -p $2 -P "$HEAVY_P"''')
rep('$R -J ' + chr(92), '$R -J; } ' + chr(92))
rep('# the heavy user starts with the probe', '''ip netns exec $IR sysctl -q -w net.ipv4.ip_local_reserved_ports=5200-5230,8443-8470,41000-41063; ip netns exec $KH sysctl -q -w net.ipv4.ip_local_reserved_ports=5200-5230,8443-8470,41000-41063; bi=0; date +%s.%N > "$W/bgstart"
for spec in ${BG:-}; do bd=${spec%%:*}; br=${spec#*:}; BR=""; [ "$bd" = d ] && BR="-R"
  ip netns exec $IR iperf3 -c 127.0.0.1 -p $((8451+bi)) -t ${BG_SECS:-20} -b ${br}M $BR -J > "$W/bg$bi.json" 2> "$W/bg$bi.err" & echo $! >> "$W/pids"
  bi=$((bi+1)); sleep 0.3; done
# the heavy user starts with the probe''')
open(sys.argv[2], 'w').write(s)

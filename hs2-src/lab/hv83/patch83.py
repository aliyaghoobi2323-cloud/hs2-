import sys
p = sys.argv[1]; s = open(p).read(); blk = open(sys.argv[2]).read()
s = s.replace("# sp82: speedtest-like client", blk + "\n# sp82: speedtest-like client", 1)
old = """if [ "${SPT:-0}" = 1 ]; then
  HPM='"port_map":"8444=127.0.0.1:5202",'; HFP=",8444\""""
new = """if [ -n "${HV_USERS:-}" ]; then
  HPM='"port_map":"8444=127.0.0.1:5202'; HFP=",8444"
  ip netns exec $KH $SPTBIN -server -listen 127.0.0.1:5202 >/dev/null 2>&1 & echo $! >> "$W/pids"
  for id in $(awk '$2=="u"{print $1}' $HV_USERS); do HPM="$HPM,$((8460+id))=127.0.0.1:$((5260+id))"; HFP="$HFP,$((8460+id))"
    ip netns exec $KH iperf3 -s -1 -B 127.0.0.1 -p $((5260+id)) >/dev/null 2>&1 & echo $! >> "$W/pids"; done
  HPM="$HPM\\","
elif [ "${SPT:-0}" = 1 ]; then
  HPM='"port_map":"8444=127.0.0.1:5202",'; HFP=",8444\""""
assert old in s; s = s.replace(old, new, 1)
old = '[ "${SPT:-0}" = 1 ] && spt_go\n'
assert old in s; s = s.replace(old, old + '[ -n "${HV_USERS:-}" ] && hv_go\n', 1)
old = '[ -n "${SPTP:-}" ] && { wait $SPTP;'
assert old in s; s = s.replace(old, '[ -n "${HV_USERS:-}" ] && hv_end\n' + old, 1)
open(p, 'w').write(s)

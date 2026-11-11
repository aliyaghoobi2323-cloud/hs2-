import sys
p=sys.argv[1]; s=open(p).read()
old='[ -n "$HEAVY" ] && heavy_go 127.0.0.1 8444\n'
assert s.count(old)==1
new='''if [ -n "$HEAVY" ] && [ "${HEAVY_CTL:-}" = direct ]; then
  # s45: the iperf3 control connection goes over the bare path (not a link, so the
  # link kill cannot close it); the data streams (--cport 41000+i) are DNATed to 8444.
  ip netns exec $KH python3 "$D/tcprelay.py" 192.168.50.2 5299 127.0.0.1 5202 >/dev/null 2>&1 & echo $! >> "$W/pids"
  ip netns exec $IR python3 "$D/tcprelay.py" 127.0.0.1 8450 192.168.50.2 5299 >/dev/null 2>&1 & echo $! >> "$W/pids"
  ip netns exec $IR iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 8450 -m multiport --sports 41000:41063 -j DNAT --to-destination 127.0.0.1:8444
  sleep 0.3
  HEAVY_XARGS="--cport 41000" heavy_go 127.0.0.1 8450
elif [ -n "$HEAVY" ]; then heavy_go 127.0.0.1 8444; fi
'''
s=s.replace(old,new)
old2='-t "$HEAVY_SECS" -O "$HEAVY_OMIT" $R -J'
assert s.count(old2)==1
s=s.replace(old2,'-t "$HEAVY_SECS" -O "$HEAVY_OMIT" ${HEAVY_XARGS:-} $R -J')
old3='''    (sleep "$HEAVY_OMIT"; ip netns exec $IR curl -s -o "$W/ir.prof" "http://127.0.0.1:6060/debug/pprof/profile?seconds=$ps") & HPP2=$!
'''
assert s.count(old3)==1
s=s.replace(old3,old3+'''    (sleep "$HEAVY_OMIT"; for k in mutex block; do ip netns exec $KH curl -s -o "$W/kh.$k.prof" "http://127.0.0.1:6060/debug/pprof/$k?seconds=$ps" & ip netns exec $IR curl -s -o "$W/ir.$k.prof" "http://127.0.0.1:6060/debug/pprof/$k?seconds=$ps" & done; wait) & HPP3=$!
''')
s=s.replace('wait "$HVP" "$HCP" ${HPP1:-} ${HPP2:-} 2>/dev/null','wait "$HVP" "$HCP" ${HPP1:-} ${HPP2:-} ${HPP3:-} 2>/dev/null')
open(p,'w').write(s)
# keep the lock profiles and heavy.json in LOGDIR (applied with sed on lab 1)
s=open(p).read()
old4='for s in ir kh; do [ -s "$W/$s.prof" ]'
if s.count(old4)==1:
    s=s.replace(old4,'[ -s "$W/heavy.json" ] && cp "$W/heavy.json" "$LOGDIR/${RUNTAG:-run}-heavy.json"; for s in ir kh ir.mutex kh.mutex ir.block kh.block; do [ -s "$W/$s.prof" ]')
    open(p,'w').write(s)

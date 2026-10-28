#!/bin/bash
# memwatch.sh OUT: while a judge block runs, the host's TCP memory each
# second (/proc/net/sockstat "TCP: ... mem P", pages, against tcp_mem), and
# for each run of RIG_ID q, 15 s after its exit hs2 starts: the skmem of
# every TCP socket of r-exitq and r-edgeq by process (ss -tmp), and the
# memory-pressure counters of both namespaces (nstat). Ends when killed.
set -u
OUT=${1:?}
mkdir -p "$OUT"
read -r lo pr hi < /proc/sys/net/ipv4/tcp_mem
echo "tcp_mem $lo $pr $hi" > "$OUT/tcp_mem.txt"
declare -A seen
while :; do
  echo "$(date +%s) $(awk '/^TCP:/{for(i=1;i<=NF;i++) if($i=="mem") print $(i+1)}' /proc/net/sockstat)" >> "$OUT/sockstat.txt"
  for p in $(pgrep -f "run -c /tmp/rig-scaleq/.*/exit.json"); do
    [ -n "${seen[$p]:-}" ] && continue
    seen[$p]=1
    (
      sleep 15
      d=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -o '/tmp/rig-scaleq/[^/]*' | head -1)
      id=${d##*/}
      f=$OUT/$id.txt
      {
        echo "== $(date -Is) $id"
        for ns in r-exitq r-edgeq; do
          echo "-- $ns skmem by process (r+w+f MB, sockets, max per socket MB)"
          ip netns exec $ns ss -tmnpH 2>/dev/null | python3 -c '
import sys, re, collections
tot = collections.Counter(); cnt = collections.Counter(); mx = collections.Counter(); cur = "?"
for l in sys.stdin:
    m = re.search(r"users:\(\(\"([^\"]+)\"", l)
    if m: cur = m.group(1)
    s = re.search(r"skmem:\(r(\d+),rb(\d+),t(\d+),tb(\d+),f(\d+),w(\d+)", l)
    if s:
        r, rb, t, tb, f, w = map(int, s.groups()); v = r + w + f
        tot[cur] += v; cnt[cur] += 1; mx[cur] = max(mx[cur], v)
for k, v in tot.most_common():
    print("  %-12s %8.1f MB %5d socks, max %.2f MB" % (k, v / 1e6, cnt[k], mx[k] / 1e6))
'
          echo "-- $ns nstat"
          ip netns exec $ns nstat -az TcpExtTCPMemoryPressures TcpExtTCPMemoryPressuresChrono TcpExtPruneCalled TcpExtTCPRcvQDrop TcpExtRcvPruned 2>/dev/null | grep -v "^#"
        done
        echo "-- sockstat"; grep TCP: /proc/net/sockstat
      } > "$f" 2>&1
    ) &
  done
  sleep 1
done

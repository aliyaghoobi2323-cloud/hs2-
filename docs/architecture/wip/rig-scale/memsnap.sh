#!/bin/bash
# memsnap.sh OUT [RIG_ID]: while a block runs, for each run (a new exit hs2 of
# the rig), every 0.5 s for its first 20 s: the host's TCP memory and, per
# namespace (edge, exit), the TCP sockets' memory (skmem r+w+f) by process
# and state, all states (ss -tmnaH). Ends when killed. R1 round 4: where the
# rig's socket memory goes.
set -u
OUT=${1:?}; ID=${2:-4}
mkdir -p "$OUT"
declare -A seen
agg() {
  python3 -c '
import sys, re, collections
tot = collections.Counter(); cnt = collections.Counter(); mx = collections.Counter()
st, who = "?", "(none)"
for l in sys.stdin:
    if l.strip() and not l[0].isspace():
        st = l.split()[0]
        m = re.search(r"users:\(\(\"([^\"]+)\"", l)
        who = m.group(1) if m else "(none)"
    s = re.search(r"skmem:\(r(\d+),rb\d+,t(\d+),tb\d+,f(\d+),w(\d+)", l)
    if not s:
        continue
    r, t, f, w = map(int, s.groups())
    k = (who, st)
    v = r + f + w
    tot[k] += v; cnt[k] += 1; mx[k] = max(mx[k], v)
for k, v in tot.most_common(8):
    print("    %-10s %-10s %8.1f MB %5d socks, max %.2f MB" % (k[0], k[1], v / 1e6, cnt[k], mx[k] / 1e6))
'
}
while :; do
  for p in $(pgrep -f "run -c /tmp/rig-scale$ID/.*/exit.json"); do
    [ -n "${seen[$p]:-}" ] && continue
    seen[$p]=1
    (
      d=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -o "/tmp/rig-scale$ID/[^/]*" | head -1)
      f=$OUT/${d##*/}.txt
      for i in $(seq 1 40); do
        {
          echo "== +$((i * 5))00ms $(date +%T.%N | cut -c1-12) host TCP $(awk '/^TCP:/{for(i=1;i<=NF;i++) if($i=="mem") print $(i+1)}' /proc/net/sockstat) pages"
          for ns in r-edge$ID r-exit$ID; do
            echo "  $ns $(ip netns exec $ns cat /proc/net/sockstat 2>/dev/null | grep TCP:)"
            ip netns exec $ns ss -tmnapH 2>/dev/null | agg
          done
        } >> "$f" 2>&1
        sleep 0.5
      done
    ) &
  done
  sleep 0.3
done

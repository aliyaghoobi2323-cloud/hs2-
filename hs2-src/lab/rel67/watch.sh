#!/bin/bash
# watch.sh: every 120 s while r67soak runs: obs_smux_stale_upd (ir/kh), ir goroutines in closeWaitEnd (fx168 H2 waiters),
# kh30 kernel RST counters (proxy for mconnTargetRst, which is not exported), panics
O=/opt/hs2lab/r67/soak; W=/tmp/r67soakw30
while systemctl is-active -q r67soak; do
  t0=$(cat $O/t0 2>/dev/null || echo 0); el=$(( $(date +%s) - t0 ))
  su(){ grep -o "\"obs_smux_stale_upd\":[0-9]*" /run/hs2/tmp-r67soakw30-$1.json.status.json 2>/dev/null | cut -d: -f2; }
  cw=$(ip netns exec ir30 curl -s -m 5 "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" 2>/dev/null | grep -B1 -E "closeWaitEnd" | grep -oE "^[0-9]+ @" | awk "{s+=\$1} END{print s+0}")
  ns=$(ip netns exec kh30 nstat -as TcpOutRsts TcpExtTCPAbortOnData TcpExtTCPAbortOnClose 2>/dev/null | awk "NR>1{printf \"%s=%s \", \$1, \$2}")
  pi=$(grep -a -c -E "panic|fatal error" $W/ir.log 2>/dev/null); pk=$(grep -a -c -E "panic|fatal error" $W/kh.log 2>/dev/null)
  echo "$(date -u +%T) el=$el stale_upd ir=${su_ir:-$(su ir)} kh=$(su kh) closewait_gor=$cw kh: $ns panics ir=$pi kh=$pk" >> $O/watch.txt
  sleep 120; done

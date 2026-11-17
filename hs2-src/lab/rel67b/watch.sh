#!/bin/bash
# watch.sh (rel67b: + rescue-rec big X KB of N MB, bigno/shrunk, keepstall idle_*): every 120 s while r67bsoak runs: obs_smux_stale_upd (ir/kh), ir goroutines in closeWaitEnd (fx168 H2 waiters),
# kh30 kernel RST counters, rel67 status counters mconn_target_rst/mconn_user_rst/stream_rst, last rescue169 log line, panics
O=/opt/hs2lab/r67b/soak; W=/tmp/r67bsoakw30
while systemctl is-active -q r67bsoak; do
  t0=$(cat $O/t0 2>/dev/null || echo 0); el=$(( $(date +%s) - t0 ))
  su(){ grep -o "\"obs_smux_stale_upd\":[0-9]*" /run/hs2/tmp-r67bsoakw30-$1.json.status.json 2>/dev/null | cut -d: -f2; }
  cw=$(ip netns exec ir30 curl -s -m 5 "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" 2>/dev/null | grep -B1 -E "closeWaitEnd" | grep -oE "^[0-9]+ @" | awk "{s+=\$1} END{print s+0}")
  ns=$(ip netns exec kh30 nstat -as TcpOutRsts TcpExtTCPAbortOnData TcpExtTCPAbortOnClose 2>/dev/null | awk "NR>1{printf \"%s=%s \", \$1, \$2}")
  rc(){ python3 -c "import json,sys
d=json.load(open(sys.argv[1]));print(*[k.replace(\"mconn_\",\"\")+\"=\"+str(d.get(k,0)) for k in (\"mconn_target_rst\",\"mconn_user_rst\",\"stream_rst\")])" /run/hs2/tmp-r67bsoakw30-$1.json.status.json 2>/dev/null; }
  r9(){ grep -a "rescue169:" $W/$1.log 2>/dev/null | tail -1 | sed "s/.*rescue169: //"; }
  pi=$(grep -a -c -E "panic|fatal error" $W/ir.log 2>/dev/null); pk=$(grep -a -c -E "panic|fatal error" $W/kh.log 2>/dev/null)
  rr(){ grep -a "rescue-rec: conns" $W/$1.log 2>/dev/null | tail -1 | grep -oE "conns [0-9]+ \([^)]*\)|bigno [0-9]+ shrunk [0-9]+ \([0-9]+ KB\)" | tr "\n" " "; }
  ks(){ grep -a "keepstall:" $W/$1.log 2>/dev/null | tail -1 | grep -oE "idle_[a-z_]+[ =][0-9]+" | tr "\n" " "; }
  echo "$(date -u +%T) el=$el stale_upd ir=${su_ir:-$(su ir)} kh=$(su kh) closewait_gor=$cw kh: $ns panics ir=$pi kh=$pk | ir $(rc ir) kh $(rc kh) | r169 ir: $(r9 ir) | kh: $(r9 kh) | rec ir: $(rr ir) kh: $(rr kh) | ksidle ir: $(ks ir) kh: $(ks kh)" >> $O/watch.txt
  sleep 120; done

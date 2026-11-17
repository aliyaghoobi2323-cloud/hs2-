#!/bin/bash
# ab81.sh: SELFQ A/B summary on one lab (res/selfA = keys off, res/selfB = HS2_LB_SELFQ=4)
cd /opt/hs2lab/lbv81/res
for t in ${TAGS:-selfA selfB}; do
  W=$(ls -d /tmp/lbv81-$t* | head -1)
  [ -e $t/kh.log ] || cp $W/ir.log $W/kh.log $W/ping.txt $t/ 2>/dev/null
  [ -e $t/result.json ] || tail -1 $t/run.out > $t/result.json
  t0=$(cat $t/t0); s0=$(date -u -d @$t0 +%s)
  echo "== $t $(cat $t/env.txt | sed 's/HS2_PPROF=127.0.0.1:6060//g')"
  # judge lowerings that name a queue, with t from t0
  grep -h "lb auto2: weight factor" $t/kh.log | grep "queue" | while read ts rest; do
    s=$(date -u -d "$(date -u -d @$t0 +%F) $ts" +%s.%N 2>/dev/null); echo "  t=$(printf %.0f $(echo "$s - $t0" | bc)) ${rest:0:150}"; done
  echo "  queue-lowerings: $(grep -h 'lb auto2: weight factor' $t/kh.log | grep -c '1.00→.*queue')"
  SHORT=1 python3 ../an81.py $t 2>/dev/null | sed -n '/by heavy window/,$p'
  python3 - $t/result.json <<'PY'
import json,sys
r=json.loads(open(sys.argv[1]).read()); p=r.get("probe") or {}
c=p.get("crowd",{}); k=p.get("call") or {}; g=p.get("game") or {}
print("  crowd p50/p99/fail", c.get("p50_ms"), c.get("p99_ms"), c.get("fail"), "| call p99", k.get("p99_ms"), k.get("error","")[:40], "| game p99", g.get("p99_ms"), "| ping p99", r.get("ping_p99"))
PY
  for f in $t/hv-d-*.json; do python3 -c "import json,sys; j=json.load(open(sys.argv[1])); print('  heavy', sys.argv[1].split('/')[-1], round(j['end']['sum_received']['bits_per_second']/1e6))" $f 2>/dev/null; done
done

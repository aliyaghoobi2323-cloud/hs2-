#!/bin/bash
# view.sh [O] — summary of the r66 soak (default /opt/hs2lab/r66/soak); works on r65s/r63c; works on r63c too:
#   bash /opt/hs2lab/r66/view.sh /opt/hs2lab/r63c/soak
O=${1:-/opt/hs2lab/r66/soak}; W=${W:-/tmp/r66soakw30}
[ "$O" = /opt/hs2lab/r63c/soak ] && W=/tmp/r63csoakw30; [ "$O" = /opt/hs2lab/st150/soak ] && W=/tmp/st150soakw30 ; [ "$O" = /opt/hs2lab/r65s/soak ] && W=/tmp/r65ssoakw30
cd $O || exit 1
now=$(date +%s); t0=$(cat t0 2>/dev/null || echo $now)
echo "== $O  $(date -u +%FT%TZ)  unit(r66soak)=$(systemctl is-active r66soak 2>/dev/null)  elapsed=$(( (now-t0)/60 )) min  $(cat DONE 2>/dev/null)"
cat env.txt
echo "== panics"
for s in ir kh; do
  if [ -e $s.panics ]; then echo "$s final: $(cat $s.panics)"
  elif [ -e $W/$s.log ]; then echo "$s live: $(grep -a -c -E 'panic|fatal error' $W/$s.log)  (log $(du -h $W/$s.log | cut -f1))"
  else echo "$s: no log"; fi; done
echo "== alarm.txt"; [ -s alarm.txt ] && tail -5 alarm.txt || echo "(none)"
echo "== RSS / goroutines / fds (every ~10 min)"
awk '{t=$1; s=$2; for(i=3;i<=NF;i++){split($i,a,"="); v[a[1]]=a[2]}
  if (s=="ir"||s=="kh") { k=int(t/600); if (!(k SUBSEP s in seen) || $0 ~ /MISSING/) { seen[k,s]=1;
    printf "%6ds %s rss=%5.1fMB gor=%s fds=%s %s\n", t, s, v["rss_kb"]/1024, v["gor"], v["fds"], ($3=="MISSING"?"MISSING":"") } } }' proc.txt 2>/dev/null | tail -60
tail -2 proc.txt 2>/dev/null | sed 's/^/last: /'
grep -c MISSING proc.txt 2>/dev/null | sed 's/^/MISSING samples: /'
echo "== ping (per-minute p50/p99 from min.txt: minute n p50 p99 fails k99 g99)"
awk '{n+=$2; f+=$5; p50[NR]=$3; p99[NR]=$4; if($4+0>m) m=$4+0}
 END{ if(!NR){print "(no samples)"; exit}
  asort(p50); asort(p99);
  printf "minutes=%d samples=%d fails=%d  median(p50)=%s  median(p99)=%s  p90(p99)=%s  max(p99)=%.1f\n", NR, n, f, p50[int(NR/2)+1], p99[int(NR/2)+1], p99[int(NR*0.9)+1], m }' min.txt 2>/dev/null
echo "last 3 min:"; tail -3 min.txt 2>/dev/null
[ -s result.json ] && python3 - <<'PY'
import json
d = json.load(open("result.json"))
print("result.json: ping p50=%s p90=%s p99=%s max=%s loss=%s over120=%s%% rss ir=%s kh=%s links_end=%s churn=%s" % tuple(d.get(k) for k in
  ("ping_p50","ping_p90","ping_p99","ping_max","ping_loss","ping_over120_pct","rss_ir_mb","rss_kh_mb","links_end","link_churn")))
PY
echo "== test downloads (every 120 s, 12 s -R)"
awk '{for(i=1;i<=NF;i++){split($i,a,"="); if(a[1]=="test_mbit") v=a[2]}
  n++; if(v=="ERR"){e++} else {s+=v; x[++k]=v}}
  END{ if(!n){print "(none)"; exit} asort(x); printf "n=%d err=%d mean=%.0f median=%s min=%s max=%s\n", n, e, (k?s/k:0), x[int(k/2)+1], x[1], x[k] }' tests.txt 2>/dev/null
tail -3 tests.txt 2>/dev/null
echo "== background users (bg.txt)"
awk '{for(i=1;i<=NF;i++){split($i,a,"="); if(a[1]=="mbit") v=a[2]} n++; if(v=="ERR") e++} END{printf "runs=%d err=%d\n", n, e}' bg.txt 2>/dev/null
echo "== idle groups (300 conns each)"
for f in $(ls -tr idle-*.jsonl 2>/dev/null); do python3 -c "
import json,sys
d=json.loads(open('$f').read().strip().splitlines()[-1])
print('%-22s ok=%3d eof=%3d reset=%d timeout=%d open_fail=%d other=%d' % ('$f'[5:-6], d['ok'], d['eof'], d['reset'], d['timeout'], d['open_fail'], d['other']))" 2>/dev/null || echo "$f (running)"; done
cat idle-*.jsonl 2>/dev/null | python3 -c "
import json,sys
t={'ok':0,'eof':0,'reset':0,'timeout':0,'n':0}
for l in sys.stdin:
  try: d=json.loads(l)
  except Exception: continue
  for k in t: t[k]+=d.get(k,0)
print('TOTAL', ' '.join('%s=%d'%kv for kv in t.items()))"
echo "== stripe steer (last counters)"
for s in ir kh; do
  if [ -e $s.steer.txt ]; then l=$(grep -a 'stripe steer:' $s.steer.txt | tail -1); u=$(grep -a -c 'lb auto2 upload' $s.steer.txt)
  elif [ -e $W/$s.log ]; then l=$(grep -a 'stripe steer:' $W/$s.log | tail -1); u=$(grep -a -c 'lb auto2 upload' $W/$s.log)
  else l=""; u=0; fi
  echo "$s: ${l:-(no stripe steer line)}"; echo "$s: 'lb auto2 upload' lines=$u"; done
echo "stripe status (t active refused subs links): $(tail -1 stripe.txt 2>/dev/null)"

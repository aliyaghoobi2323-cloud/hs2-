#!/bin/bash
# sum127.sh RUN...: rk127 summary per run (lab3 /opt/hs2lab/rel62k/res/RUN).
set -u
for r in "$@"; do
  O=/opt/hs2lab/rel62k/res/$r; [ -d $O ] || { echo "== $r missing"; continue; }
  lg(){ if [ -e $O/ir.sel.gz ]; then zcat $O/ir.sel.gz; else cat /tmp/rk127-$r-*/ir.log /tmp/mx127-$r-*/ir.log 2>/dev/null; fi; }
  echo "== $r $(cat $O/env.txt) $( [ -e $O/DONE ] && echo DONE || echo RUNNING)"
  for f in $O/storm-g*.jsonl; do [ -e $f ] && printf '%-6s %s\n' $(basename $f .jsonl | cut -c7-) "$(python3 -c 'import json,sys;j=json.loads(open(sys.argv[1]).read().splitlines()[-1]);print({k:v for k,v in j.items() if "fail" in k or "err" in k or k in("n","ok")})' $f 2>/dev/null || cut -c1-200 $f)"; done
  for f in $O/storm-st*.jsonl; do [ -e $f ] && printf '%-6s %s\n' $(basename $f .jsonl | cut -c7-) "$(cut -c1-160 $f)"; done
  lg | grep -a -oE 'closed [0-9]+ connection\(s\) idle[^(]*' | awk '{n+=$2} END{printf "edge idle closes on retiring links: %d conns / %d lines\n", n, NR}'
  lg | grep -a -c 'retire keep:' | sed 's/^/retire keep lines: /'
  lg | grep -a 'retire keep:' | grep -av ' 0 on old rules' | tail -3 | cut -c1-220
  echo "up/serving/retiring (max per 5 min):"
  lg | grep -a -oE '^[0-9:]{5}.*\(([0-9]+) up: ([0-9]+) serving, ([0-9]+) retiring' | sed -E 's/^([0-9]{2}:[0-9]).*\(([0-9]+) up: ([0-9]+) serving, ([0-9]+) retiring/\1 \2 \3 \4/' | awk '{k=$1; if($2>u[k])u[k]=$2; if($4>t[k])t[k]=$4} END{for(k in u) printf "%sx up<=%d ret<=%d; ", k, u[k], t[k]; print ""}' | tr ';' '\n' | sort | tr '\n' ';'; echo
  awk '$2=="ir" && NR%20==1 {printf "%s:%dMB/%sfd/%sgr ", $1, substr($4,5)/1024, substr($5,5), substr($6,4)}' $O/proc.txt; echo
  cat $O/ir.panics $O/kh.panics 2>/dev/null | tr '\n' ' '; echo "(panics ir kh)"
  tail -c 800 $O/result.json 2>/dev/null | tr ',' '\n' | grep -iE 'p99|fail' | head -8 | tr '\n' ' '; echo
  cut -c1-50 $O/heavy.txt 2>/dev/null | tr '\n' ';'; echo
done

#!/bin/bash
# lossmap.sh [soakdir] [workdir|-] — map hs0 ping losses (seq gaps) and "tun side channel" drops to the event phase (elapsed % 1800)
O=${1:-/opt/hs2lab/r67/soak}; W=${2:-/tmp/r67soakw30}; t0=$(cat $O/t0)
ev(){ awk -v p=$1 "BEGIN{s=\"steady\"} {if(p>=\$1%1800 && p<\$2%1800+ (\$2%1800<\$1%1800?1800:0)) s=(s==\"steady\"?\$3:s\"+\"\$3)} END{print s}" <(head -12 $O/events.txt); }
P=$W/ping.txt; [ -s $P ] || P=$O/ping.copy.txt
echo "== ping gaps ($P, seq*0.2 s from t0)"
[ -s $P ] && awk -F"icmp_seq=" "/icmp_seq/{split(\$2,a,\" \"); s=a[1]+0; if(last && s>last+1) print last+1, s-last-1; last=s}" $P | while read s n; do e=$(( (s*2/10) )); echo "el=${e}s phase=$((e%1800)) lost=$n $(ev $((e%1800)))"; done
tail -2 $P | grep -a -E "transmitted|loss"
echo "== tun side channel drops (log time = end of 30 s window)"
for s in ir kh; do f=$W/$s.log; [ -e $f ] || { zcat $O/logs/$s.full.log.gz > /tmp/lm.$s 2>/dev/null; f=/tmp/lm.$s; }
  grep -a "tun side channel" $f | while read ts rest; do d=$(date -u +%F); e=$(( $(date -u -d "$d $ts" +%s) - t0 )); n=$(echo "$rest" | grep -o "dropped [0-9]*"); echo "$s el=${e}s phase=$((e%1800)) $n window=$(ev $(( (e-15)%1800 )))"; done; done

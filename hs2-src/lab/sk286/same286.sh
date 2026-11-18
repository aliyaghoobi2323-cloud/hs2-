#!/bin/bash
# per run: Iran call trace lines with 2+ subs where two subs ride one path (lines / calls), and of those lines with stalled>=1s
cd /opt/hs2lab/dc254
for s in "$@"; do
 zcat $s/logs/ir.full.log.gz | grep -aE "darkcall ${TR:-l.?trace} iran=true" | awk -v s=$s "{
  split(\$0,P,\" [|] \"); n=split(P[2],T,\" \"); delete seen; dup=0
  for(i=1;i<=n;i++){ x=T[i]; sub(/^[0-9]+:/,\"\",x); sub(/>.*/,\"\",x); if(x in seen)dup=1; seen[x]=1 }
  if(n>=2){tot++; if(dup){same++; c[\$2]=1; if(match(\$0,/stalled [0-9]+ ms/) && substr(\$0,RSTART+8,RLENGTH-11)+0>=1000)ss++}}
 } END{nc=0; for(k in c)nc++; printf \"%s two-sub lines %d samepath %d (%.2f%%) calls %d samepath+stalled>=1s %d\n\", s, tot, same, 100*same/(tot?tot:1), nc, ss}"
done

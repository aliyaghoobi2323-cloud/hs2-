#!/bin/bash
# firstn.sh DIR MIN — compare soaks over the same first MIN minutes
O=$1; M=$2
awk -v M=$M '$1<M{n+=$2; f+=$5; k++; p50[k]=$3; p99[k]=$4; if($4+0>m)m=$4+0} END{asort(p50);asort(p99); printf "ping min=%d samples=%d fails=%d med(p50)=%s med(p99)=%s p90(p99)=%s max(p99)=%.1f\n",k,n,f,p50[int(k/2)+1],p99[int(k/2)+1],p99[int(k*0.9)+1],m}' $O/min.txt
awk -v M=$M '$1<M*60{for(i=1;i<=NF;i++){split($i,a,"="); if(a[1]=="test_mbit") v=a[2]} n++; if(v=="ERR")e++; else {s+=v; x[++k]=v}} END{asort(x); printf "tests n=%d err=%d mean=%.0f median=%s min=%s max=%s\n",n,e,(k?s/k:0),x[int(k/2)+1],x[1],x[k]}' $O/tests.txt
awk -v M=$M '$1<M*60{for(i=1;i<=NF;i++){split($i,a,"="); if(a[1]=="mbit") v=a[2]} n++; if(v=="ERR")e++} END{printf "bg runs=%d err=%d\n",n,e}' $O/bg.txt
awk -v M=$M '$1<M*60 && ($2=="ir"||$2=="kh"){split($4,a,"="); if(a[2]>mx[$2])mx[$2]=a[2]; last[$2]=$0} END{printf "rss max ir=%.0fMB kh=%.0fMB\n", mx["ir"]/1024, mx["kh"]/1024}' $O/proc.txt

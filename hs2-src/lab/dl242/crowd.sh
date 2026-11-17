awk -v a=$a '{s+=$4;n++;if($4>m)m=$4;f+=$5}END{printf "%s crowd p99 avg %.1f max %.1f fails %d\n",a,s/n,m,f}' $a/min.txt

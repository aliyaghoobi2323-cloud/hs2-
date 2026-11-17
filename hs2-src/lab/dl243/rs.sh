#!/bin/bash
cd /opt/hs2lab/dl243
for a in "$@"; do f=$a/logs/kh.full.log.gz
 r=$(zgrep -c "young sub [0-9]* rescued" $f); fa=$(zgrep -c "class fast" $f); sl=$(zgrep -c "class slow" $f); st=$(zgrep -c "class stuck" $f)
 kb=$(zgrep -oE "[0-9]+ chunks [0-9]+ KB from off" $f | awk '{s+=$3}END{print s+0}')
 last=$(zgrep -oE "ok [0-9]+ slowOK [0-9]+ fast [0-9]+ slow [0-9]+ capped [0-9]+" $f | tail -1)
 echo "$a rescues $r fast $fa slow $sl stuck $st KB $kb | last: $last"
done

#!/bin/bash
cd /opt/hs2lab/dl242
for a in "$@"; do
python3 an.py $a
python3 young2.py $a/logs/ir.full.log.gz | tail -1
python3 -c "import json;d=json.load(open(\"$a/result.json\"));print(\"$a ping p99\",d.get(\"ping_p99\"),\"max\",d.get(\"ping_max\"),\"loss\",d.get(\"ping_loss\"),\"rss kh\",d.get(\"rss_kh_mb\"))"
. ./crowd.sh
echo "$a fails $(wc -l < $a/fails.txt) panics ir $(cat $a/ir.panics) kh $(cat $a/kh.panics) | rescue lines $(zgrep -c "dl242: young sub" $a/logs/kh.full.log.gz) | iran ticks oooB>=8MB $(zgrep -E "tick iran=true .*oooB=[0-9]+" $a/logs/ir.full.log.gz | grep -oE "oooB=[0-9]+" | awk -F= "\$2>=8388608" | wc -l) | gapCheck resets $(zgrep -c "missing bytes never arrived" $a/logs/ir.full.log.gz)"
done

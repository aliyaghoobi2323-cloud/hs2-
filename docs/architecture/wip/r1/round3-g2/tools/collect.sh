#!/bin/bash
# collect.sh BLOCK...: a block's results into the repo (round3-g2/smoke/BLOCK):
# res.jsonl gzipped, run.log, orch.log, plan, host, ana.py's verdicts, the
# g2table.py table and g2lines.py's lines; no raw/ (it holds the rig's
# addresses).
set -u
W=/root/hs2-wt/r1-r3b/docs/architecture/wip/r1/round3-g2
for b in "$@"; do
  S=/root/r3b/$b; O=$W/smoke/$b
  [ -f $S/res.jsonl ] || { echo "$b: no res.jsonl"; continue; }
  mkdir -p $O
  gzip -9c $S/res.jsonl > $O/res.jsonl.gz
  [ -f $S/res.dropped.jsonl ] && gzip -9c $S/res.dropped.jsonl > $O/res.dropped.jsonl.gz
  for f in run.log orch.log plan.txt host.txt schedule.tsv; do [ -f $S/$f ] && cp $S/$f $O/; done
  RS=${RS:-/root/r3b/rig}
  python3 $RS/ana.py $S/res.jsonl > $O/ana.txt 2>&1
  RIGSCALE=$RS python3 /root/r3b/tools/g2table.py $S/res.jsonl > $O/table.md 2>&1
  python3 /root/r3b/tools/g2lines.py $S/res.jsonl > $O/lines.txt 2>&1
  du -sh $O
done

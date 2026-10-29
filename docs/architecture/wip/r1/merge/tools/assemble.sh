#!/bin/bash
# assemble.sh: the R1 merge's (step 5) data into the repo,
# docs/architecture/wip/r1/merge/ (no raw run files, nothing over 5 MB):
# suite summaries, step-5 script results, rig blocks (plan, schedule, logs,
# host snapshots, res.jsonl gzipped, guard logs, proc/table/class/ana), the
# tools, the locked time, the release sums. Files are written in place.
set -u
D=/root/hs2-wt/r1-merge/docs/architecture/wip/r1/merge
mkdir -p "$D/suite" "$D/step5" "$D/tools" "$D/logs"
cp /root/rmerge/suite/*.txt "$D/suite/" 2>/dev/null
for f in /root/rmerge/step5/*.txt; do [ -f "$f" ] && cp "$f" "$D/step5/"; done
for b in RS RC RF; do
  s=/root/rmerge/$b; [ -d "$s" ] || continue
  o=$D/$b; mkdir -p "$o"
  for f in plan.txt schedule.tsv run.log host.txt bins.txt bins-sha256.txt proc.md table.md class.md ana.txt; do [ -f "$s/$f" ] && cp "$s/$f" "$o/"; done
  for f in res.jsonl res.dropped.jsonl; do [ -s "$s/$f" ] && gzip -c "$s/$f" > "$o/$f.gz"; done
  for g in "$s"/guard-*; do [ -d "$g" ] && cp "$g/guard.log" "$o/$(basename "$g").log" 2>/dev/null; done
done
cp /root/rmerge/logs/*.txt "$D/logs/" 2>/dev/null
cp /root/rmerge/tools/*.sh /root/rmerge/tools/*.py "$D/tools/"
cp /root/rmerge/plans/*.txt "$D/tools/" 2>/dev/null
cp /root/rmerge/locktime.log "$D/locktime.log"
H=$(git -C /root/hs2-wt/r1-merge rev-parse --short HEAD)
[ -f /root/rmerge/raw-dist-$H/SHA256SUMS ] && cp /root/rmerge/raw-dist-$H/SHA256SUMS "$D/SHA256SUMS"
find "$D" -type f -size +5M -print
du -sh "$D"

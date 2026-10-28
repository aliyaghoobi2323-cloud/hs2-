#!/bin/bash
# assemble.sh: R1 round 4's data into the repo (docs/architecture/wip/r1/round4/,
# no raw run files): per block its plan, schedule, orch and run logs, host
# snapshots, results (res.jsonl gzipped), the guard's logs, and the tables
# (class.md: user classes; table.md: P10 and latency; ramp/cdip where they
# apply); the tests' outputs; the tools; the binaries' labels and sha256.
set -u
D=/root/hs2-wt/r1-r4/docs/architecture/wip/r1/round4
T=/root/r4/tools
mkdir -p "$D/tools" "$D/tests"
for b in A1 A2 A3 A4 A5 A6 A7 D1 R1 R2a R2b R3 R4 R5 R6; do
  s=/root/r4/$b; [ -d "$s" ] || continue
  o=$D/$b; mkdir -p "$o"
  for f in plan.txt schedule.tsv orch.log run.log host.txt; do [ -f "$s/$f" ] && cp "$s/$f" "$o/"; done
  for f in res.jsonl res.dropped.jsonl; do [ -s "$s/$f" ] && gzip -c "$s/$f" > "$o/$f.gz"; done
  for g in "$s"/guard-* /root/r4/raw-$b/guard-*; do
    [ -d "$g" ] || continue
    mkdir -p "$o/guard"
    cp "$g/guard.log" "$o/guard/$(basename "$g").log" 2>/dev/null
    [ -f "$g/memwatch.tsv" ] && gzip -c "$g/memwatch.tsv" > "$o/guard/$(basename "$g")-memwatch.tsv.gz"
  done
  if [ -s "$s/res.jsonl" ]; then
    python3 $T/r4class.py "$s/res.jsonl" > "$o/class.md" 2>/dev/null
    python3 $T/r4table.py "$s/res.jsonl" > "$o/table.md" 2>/dev/null
    python3 $T/cdip.py "$s/res.jsonl" > "$o/cdip.md" 2>/dev/null
    [ -s "$o/cdip.md" ] && [ "$(wc -l < "$o/cdip.md")" -le 2 ] && rm -f "$o/cdip.md"
    python3 $T/hyp.py "$s/res.jsonl" > "$o/hyp.md" 2>/dev/null
    [ -s "$o/hyp.md" ] && [ "$(wc -l < "$o/hyp.md")" -le 2 ] && rm -f "$o/hyp.md"
  fi
done
[ -d /root/r4/D1-mem ] && { mkdir -p "$D/D1/mem"; cp /root/r4/D1-mem/*.txt "$D/D1/mem/"; }
[ -d /root/r4/DPI ] && { mkdir -p "$D/DPI"; for f in /root/r4/DPI/*; do case "$f" in */rec-*.json) ;; *) cp "$f" "$D/DPI/" ;; esac; done; }
cp /root/r4/tests/*.txt "$D/tests/" 2>/dev/null
for f in block.sh chain.sh capture.sh testwin.sh withsnap.sh hyp.py cdip.py r4class.py r4table.py assemble.sh; do cp "$T/$f" "$D/tools/" 2>/dev/null; done
cp /root/r4/gtest/fillsock.py "$D/tools/" 2>/dev/null
cp /root/r4/bins.txt "$D/bins.txt"
( cd /root/r4/bins && sha256sum * ) > "$D/bins-sha256.txt"
cp /root/r4/locktime.log "$D/locktime.log"
cp -r /root/r4/plans "$D/"
find "$D" -type f -size +5M -print
du -sh "$D"

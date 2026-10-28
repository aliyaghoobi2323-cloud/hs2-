#!/bin/bash
# assemble.sh: the judge's data for the repo (docs/architecture/wip/r1/
# round3-judge/), from /root/r3j: per block its plan, schedule, orch.log,
# run.log, host.txt, res.jsonl.gz (and the dropped runs), its tables
# (j3table.py --compact, classtable.py), the memory summaries; the xray
# blocks' results and tables; the DPI outputs; the test window's logs; the
# binaries' list and sha256; the locked time; the tools and plans. No raw
# run files, no keys, no addresses.
set -u
D=${1:-/root/hs2-wt/r1-r3j/docs/architecture/wip/r1/round3-judge}
S=/root/r3j
mkdir -p "$D"
cd "$S"
for b in P1 D1 D2 P2a C1 C2 C3 D3 D4 D5 D6 C4 C5 F1 F2 F3 F4 F5 F6 F2b; do
  [ -d "$b" ] || continue
  mkdir -p "$D/$b"
  for f in plan.txt schedule.tsv orch.log run.log host.txt res.dropped.jsonl; do
    [ -f "$b/$f" ] && cp "$b/$f" "$D/$b/"
  done
  if [ -s "$b/res.jsonl" ]; then
    gzip -9c "$b/res.jsonl" > "$D/$b/res.jsonl.gz"
    python3 tools/j3table.py --compact "$b/res.jsonl" > "$D/$b/table.md" 2>&1
    python3 tools/classtable.py "$b/res.jsonl" > "$D/$b/class.md" 2>&1
  fi
  if [ -d "$b-mem" ]; then
    mkdir -p "$D/$b/mem"
    for f in "$b-mem"/*.txt; do gzip -9c "$f" > "$D/$b/mem/$(basename "$f").gz"; done
  fi
done
for x in X1 X2 X3; do
  [ -d "$x/out" ] || continue
  mkdir -p "$D/$x"
  cp "$x/orch.log" "$D/$x/" 2>/dev/null
  for f in plan.txt run.log; do [ -f "$x/out/$f" ] && cp "$x/out/$f" "$D/$x/"; done
  [ -s "$x/out/res.jsonl" ] && gzip -9c "$x/out/res.jsonl" > "$D/$x/res.jsonl.gz" && \
    python3 /root/hs2-wt/r1-r3j/docs/architecture/wip/r1/xray/xana.py table "$x/out/res.jsonl" --md > "$D/$x/table.md" 2>&1
done
if [ -d DPI ]; then
  mkdir -p "$D/dpi"
  for f in DPI/*; do
    case "$f" in *.json) gzip -9c "$f" > "$D/dpi/$(basename "$f").gz" ;; *) cp "$f" "$D/dpi/" ;; esac
  done
fi
[ -d D1 ] && [ -f D1/gdump.txt ] && cp D1/gdump.txt "$D/D1/"
for t in tests-*; do [ -d "$t" ] && mkdir -p "$D/tests" && cp "$t"/*.txt "$D/tests/"; done
mkdir -p "$D/plans" "$D/tools"
cp plans/*.txt "$D/plans/"
cp tools/*.sh tools/*.py "$D/tools/" 2>/dev/null
rm -f "$D/tools/mkj3table.py"
cp bins.txt locktime.log buildtime.log "$D/"
(cd bins && sha256sum hs2-*) > "$D/bins-sha256.txt"
du -sh "$D"; find "$D" -size +4M

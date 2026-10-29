#!/bin/bash
# assemble.sh: the R1 round-4 judge's data into the repo
# (docs/architecture/wip/r1/round4-judge/, no raw run files): per block its
# plan, schedule, orch and run logs, host snapshots, results (res.jsonl
# gzipped), the guard's logs, and the tables (class.md: user classes;
# table.md: P10 and latency; cdip.md: P10 (c)'s seconds where it applies);
# the DPI block without pcaps or record lists; the tests' outputs; the
# tools, the plans, the binaries' labels and sha256, the locked time.
# Nothing is deleted: a file is written or overwritten in place.
set -u
D=/root/hs2-wt/r1-r4j/docs/architecture/wip/r1/round4-judge
T=/root/r4j/tools
export RIGSCALE=/root/hs2-wt/r1-r4j/docs/architecture/wip/rig-scale
mkdir -p "$D/tools" "$D/tests"
for b in S1 S2 S3 S4 S5 S7 S9 S10 G1 G1b G2 G5 G9 G10 H1 H2 H3 H4 H5 G4 G7 G8 S6; do
  s=/root/r4j/$b; [ -d "$s" ] || continue
  o=$D/$b; mkdir -p "$o"
  for f in plan.txt schedule.tsv orch.log run.log host.txt; do [ -f "$s/$f" ] && cp "$s/$f" "$o/"; done
  for f in res.jsonl res.dropped.jsonl; do [ -s "$s/$f" ] && gzip -c "$s/$f" > "$o/$f.gz"; done
  for g in "$s"/guard-*; do
    [ -d "$g" ] || continue
    mkdir -p "$o/guard"
    cp "$g/guard.log" "$o/guard/$(basename "$g").log" 2>/dev/null
    [ -f "$g/memwatch.tsv" ] && gzip -c "$g/memwatch.tsv" > "$o/guard/$(basename "$g")-memwatch.tsv.gz"
  done
  if [ -s "$s/res.jsonl" ]; then
    python3 $T/r4class.py "$s/res.jsonl" > "$o/class.md" 2>/dev/null
    python3 $T/r4table.py "$s/res.jsonl" > "$o/table.md" 2>/dev/null
    python3 "$RIGSCALE/ana.py" "$s/res.jsonl" > "$o/ana.txt" 2>/dev/null
    case "$b" in G1|G1b|G2|H1|H2|S2|S9|S10) python3 $T/cdip.py "$s/res.jsonl" > "$o/cdip.md" 2>/dev/null ;; esac
  fi
done
if [ -d /root/r4j/DPIr ]; then
  mkdir -p "$D/DPIr"
  for f in /root/r4j/DPIr/*; do case "$f" in */rec-*.json) ;; *) cp "$f" "$D/DPIr/" ;; esac; done
fi
for f in /root/r4j/tunbisect.txt /root/r4j/gaptest-S3.txt /root/r4j/gapbuild.txt; do [ -f "$f" ] && cp "$f" "$D/tests/"; done
cp /root/r4j/tests/*.txt "$D/tests/" 2>/dev/null
for f in block.sh chain.sh gchain.sh gchain2.sh hchain.sh after.sh capture.sh testwin.sh tunbisect.sh gaptest.sh gapbuild.sh vmmixfin.sh idlerec.py webrows.py mkplans.py mkplans2.py cdip.py r4class.py r4table.py assemble.sh; do cp "$T/$f" "$D/tools/" 2>/dev/null; done
cp /root/r4j/bins.txt "$D/bins.txt"
cp /root/r4j/bins-notes.txt "$D/bins-notes.txt"
( cd /root/r4j/bins && sha256sum * ) > "$D/bins-sha256.txt"
cp /root/r4j/locktime.log "$D/locktime.log"
cp /root/r4j/chain.log "$D/chain.log"
mkdir -p "$D/plans"; cp /root/r4j/plans/*.txt "$D/plans/"
find "$D" -type f -size +5M -print
du -sh "$D"

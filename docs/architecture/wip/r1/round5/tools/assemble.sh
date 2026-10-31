#!/bin/bash
# assemble.sh BLOCK...: R1 round 5's data of each block into the worktree
# (docs/architecture/wip/r1/round5/BLOCK/), no raw file: plan, schedule,
# orchestration and run logs, host snapshot, the guard's summary, res.jsonl
# (gzip), and the tables: ana.py (verdicts), r5class.py (classes), and for a
# block traced with TRACE_SS the link sockets' summary of each run
# (qtrace.py, qtrace.tsv). From the red team's (second pass) assemble.sh.
set -u
R=/root/hs2-wt/r1-r5; D=$R/docs/architecture/wip/r1/round5; RS=$R/docs/architecture/wip/rig-scale
for b in "$@"; do
	O=$D/$b; mkdir -p "$O"
	S=/root/r5/$b; W=/root/r5/raw-$b
	for f in plan.txt orch.log schedule.tsv host.txt; do [ -f "$S/$f" ] && cp "$S/$f" "$O/"; done
	[ -f "$W/run.log" ] && cp "$W/run.log" "$O/run.log"
	grep -h "end:\|ABORT" "$S"/guard-*/guard.log > "$O/guard.txt" 2>/dev/null
	if [ -f "$W/res.jsonl" ]; then
		gzip -c "$W/res.jsonl" > "$O/res.jsonl.gz"
		python3 "$RS/ana.py" "$W/res.jsonl" > "$O/ana.txt" 2>&1
		python3 "$D/tools/r5class.py" "$W/res.jsonl" > "$O/class.md" 2>&1
	fi
	if ls "$W"/raw/*/sstrace-exit.txt > /dev/null 2>&1; then
		printf "run\tend\tclass\tcc\tgoodput_mbit\tcwnd_mean\tcwnd_p10\tq_mean_ms\tsendq_mean_kb\tretr_per_s\tretr_share_pct\tapp_limited_share\n" > "$O/qtrace.tsv"
		for r in "$W"/raw/*/; do
			[ -f "$r/sstrace-exit.txt" ] || continue
			python3 "$D/tools/qtrace.py" "$r" | python3 -c '
import json, sys
d = json.load(sys.stdin); run = sys.argv[1]
for side in sorted(d):
    for cls in sorted(d[side]):
        x = d[side][cls]
        print("\t".join(str(v) for v in (run, side, cls, x["cc"], x["goodput_mbit"], x["cwnd_mean"], x["cwnd_p10"], x["q_mean_ms"], x["sendq_mean_kb"], x["retr_per_s"], x["retr_share_pct"], x["app_limited_share"])))
' "$(basename "$r")" >> "$O/qtrace.tsv"
		done
	fi
	for r in "$W"/raw/*/; do
		[ -f "$r/status-exit.txt" ] || continue
		echo "== $(basename "$r")"
		grep -hE "place:|realtime|hostq link|quiet cc|quiet records|budget link" "$r/status-edge.txt" "$r/status-exit.txt" | sed 's/^ *cores: *//' | cut -c1-300
	done > "$O/status-lines.txt" 2>/dev/null
	find "$O" -type f -size +4M -exec ls -la {} \;
done

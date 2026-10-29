#!/bin/bash
# assemble.sh BLOCK...: the R1 red team's (second pass) data of each block into
# the worktree (docs/architecture/wip/r1/redteam2/BLOCK/), no raw file: plan,
# schedule, orchestration and run logs, host snapshot, the guard's summary,
# res.jsonl (gzip), and the tables (ana.py, r2class.py, placelines.py,
# flap.py). For a capture block (B4dpi, B4b): the capture log, the record
# tables of idlerec.py and the connection profiles. For UG: each scenario's
# result and safe-apply logs.
set -u
R=/root/hs2-wt/r1-red2; D=$R/docs/architecture/wip/r1/redteam2; RS=$R/docs/architecture/wip/rig-scale
for b in "$@"; do
	O=$D/$b; mkdir -p "$O"
	S=/root/rred2/$b; W=/root/rred2/raw-$b
	case "$b" in
	B4dpi | B4b)
		cp "$S/capture.log" "$O/" 2>/dev/null
		python3 $R/docs/architecture/wip/r1/round4-judge/tools/idlerec.py "$S"/rec-c*.json > "$O/idle.md" 2>&1
		cp "$S"/conn-*.md "$S/dpi_eval2.txt" "$S/extract.log" "$O/" 2>/dev/null
		;;
	UG*)
		for s in "$S"/*/; do
			[ -d "$s" ] || continue
			n=$(basename "$s"); mkdir -p "$O/$n"
			cp "$s"/result.txt "$s"/safe-apply-*.log "$s"/safe-apply-*.last "$s"/events*.txt "$O/$n/" 2>/dev/null
			for f in "$s"/hs2-*.log; do [ -f "$f" ] && grep -iE "refus|upgrade|v2|older|plain website|up |links|listening|dial" "$f" | head -60 > "$O/$n/$(basename "$f" .log)-lines.txt"; done
			gzip -c "$s/probe.log" > "$O/$n/probe.log.gz" 2>/dev/null
		done
		cp "$S/ug.log" "$O/" 2>/dev/null
		;;
	*)
		for f in plan.txt orch.log run.log schedule.tsv host.txt pre.log; do [ -f "$S/$f" ] && cp "$S/$f" "$O/"; done
		[ -f "$W/run.log" ] && cp "$W/run.log" "$O/run.log"
		grep -h "end:\|ABORT" "$S"/guard-*/guard.log > "$O/guard.txt" 2>/dev/null
		if [ -f "$W/res.jsonl" ]; then
			gzip -c "$W/res.jsonl" > "$O/res.jsonl.gz"
			python3 "$RS/ana.py" "$W/res.jsonl" > "$O/ana.txt" 2>&1
			python3 "$D/tools/r2class.py" "$W/res.jsonl" > "$O/class.md" 2>&1
			python3 "$D/tools/placelines.py" "$W" > "$O/place.md" 2>&1
			python3 "$D/tools/flap.py" "$W" > "$O/flap.md" 2>&1
		fi
		;;
	esac
	find "$O" -type f -size +4M -exec ls -la {} \;
done

#!/bin/bash
# rigbody.sh NAME PLAN [REPS]: the body of one locked rig block of the R1 merge
# (run by lockrun.sh, which holds the lock and bounds the time). Builds the hs2
# binary of r1-merge's head (BUILD.md release flags) and rigload if missing,
# runs the plan with the rig's run.sh (RIG_ID 8, PROC_TRACE 5 s, POST_IDLE 20 s,
# memory guard log-only), reruns failed runs once while time is left, then
# writes the tables. Raw: /root/rmerge/raw-NAME; results: /root/rmerge/NAME.
set -u
NAME=${1:?}; PLAN=${2:?}; REPS=${3:-1}
export PATH=$PATH:/usr/local/go/bin
W=/root/hs2-wt/r1-merge; RS=$W/docs/architecture/wip/rig-scale
O=/root/rmerge/$NAME; R=/root/rmerge/raw-$NAME
mkdir -p "$O" "$R"
T0=$(date +%s); BUDGET=${BUDGET:-1380}
H=$(git -C "$W" rev-parse --short HEAD)
B=/root/rmerge/raw-bins/hs2-m-$H; RLB=/root/rmerge/raw-bins/rigload-$H
# a later commit that left hs2-src (rigload) alone runs the binary already
# built for the commit it equals there (the same bytes as the release build)
for c in $(git -C "$W" rev-list --first-parent -30 HEAD); do
  cs=$(git -C "$W" rev-parse --short "$c")
  [ -x "$B" ] || { [ -x "/root/rmerge/raw-bins/hs2-m-$cs" ] && git -C "$W" diff --quiet "$c" HEAD -- hs2-src && B=/root/rmerge/raw-bins/hs2-m-$cs; }
  [ -x "$RLB" ] || { [ -x "/root/rmerge/raw-bins/rigload-$cs" ] && git -C "$W" diff --quiet "$c" HEAD -- docs/architecture/wip/rig-scale/rigload && RLB=/root/rmerge/raw-bins/rigload-$cs; }
done
[ -x "$B" ] || (cd "$W/hs2-src" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$B" ./cmd/hs2) || exit 1
[ -x "$RLB" ] || (cd "$RS/rigload" && CGO_ENABLED=0 go build -o "$RLB" .) || exit 1
echo "m $B" > "$O/bins.txt"
# EXTRA_BINS=FILE: more labels for an A/B cell ("LABEL PATH [HS2_K=V ...]")
[ -n "${EXTRA_BINS:-}" ] && cat "$EXTRA_BINS" >> "$O/bins.txt"
sha256sum "$B" "$RLB" > "$O/bins-sha256.txt"
cp "$RLB" "$R/rigload"
tr -d '\r' < "$PLAN" > "$O/plan.txt"
hostsnap() { echo "== $1 $(date -Is)"; uptime; grep TCP: /proc/net/sockstat; grep -E "^TcpExt:" /proc/net/netstat | awk 'NR==1{for(i=2;i<=NF;i++)n[i]=$i} NR==2{for(i=2;i<=NF;i++) if(n[i]=="TCPMemoryPressures") print "TCPMemoryPressures " $i}'; top -bn2 -d0.5 -w 200 | awk "/^top -/{f++} f==2" | sed -n "1,5p;7,14p" | cut -c1-120; }
hostsnap start > "$O/host.txt"
echo "== $NAME head $H, binary $(cut -c1-12 "$O/bins-sha256.txt" | head -1), plan $(grep -vc "^#" "$O/plan.txt") cells, REPS $REPS"
left() { echo $(( T0 + BUDGET - $(date +%s) )); }
run() {
  REPS=$REPS RIG_ID=8 BINS=$O/bins.txt PROC_TRACE=5 POST_IDLE=${POST_IDLE:-20} timeout -s TERM "$(left)" "$RS/run.sh" "$O/plan.txt" "$R" >> "$O/run.out" 2>&1
  echo "-- run.sh status $? ($(left) s left)"
}
run
n=$(python3 - "$R/res.jsonl" <<'PY'
import json, sys
p = sys.argv[1]
keep, out = [], []
try:
    lines = open(p).read().splitlines()
except FileNotFoundError:
    lines = []
for l in lines:
    if l.strip():
        (keep if json.loads(l).get("ok") else out).append(l)
if out:
    open(p[:-6] + ".dropped.jsonl", "a").write("\n".join(out) + "\n")
    open(p, "w").write("\n".join(keep) + ("\n" if keep else ""))
print(len(out))
PY
)
echo "-- $n runs failed"
[ "$n" -gt 0 ] && [ "$(left)" -gt 200 ] && run
hostsnap end >> "$O/host.txt"
for f in res.jsonl res.dropped.jsonl run.log schedule.tsv; do [ -f "$R/$f" ] && cp "$R/$f" "$O/"; done
for g in "$R"/guard-*; do [ -d "$g" ] && cp -r "$g" "$O/"; done
grep -h "ABORT\|end:" "$R"/guard-*/guard.log 2>/dev/null | sed "s/^/-- guard: /"
python3 /root/rmerge/tools/procsum.py "$R" > "$O/proc.md" 2>&1
RIGSCALE=$RS python3 /root/rmerge/tools/r4table.py "$R/res.jsonl" > "$O/table.md" 2>&1
RIGSCALE=$RS python3 /root/rmerge/tools/r4class.py "$R/res.jsonl" > "$O/class.md" 2>&1
python3 "$RS/ana.py" "$R/res.jsonl" > "$O/ana.txt" 2>&1
cat "$O/proc.md"
echo "== $NAME done after $(( $(date +%s) - T0 )) s"

#!/bin/bash
# ab.sh CELLS OUT REP... — the A/B of the CPU split (R1 speed), inside one
# rig-lock block. For each repeat REP and each cell (one PLAN line a line of
# CELLS) it runs the cell once more in each condition of CONDS ("A B C"), in
# an order rotated by the repeat (repeat 1: A B C, 2: B C A, 3: C A B):
#   A  quiet host, today's rule: the load frozen, the rig unpinned
#   B  the rig pinned to RIG_CPUS (0-5) and the load running on the test cores
#   C  the rig unpinned and the load running (agents that do not pin yet)
#   D  the rig pinned and the load frozen (what pinning alone does)
# The load is the scope LOADU (r1cpu-load.scope, load.sh through r1-cpu.sh),
# frozen and thawed here with systemctl. Each condition has its own run.sh
# OUT (OUT/A, OUT/B, ...); each call adds exactly one run of the cell to it
# (REPS = the ok runs it has + 1).
# LOAD_MAX=1000: the 1-minute load average lags the freeze by minutes, so
# the quiet runs are checked by what each CPU did in the measuring window
# instead (the "cpus" field, ana.py compose).
set -u
D=${D:-/root/hs2-wt/r1-cpu/docs/architecture/wip/rig-scale}
CELLS=${1:?usage: ab.sh CELLS OUT REP...}; OUT=${2:?}; shift 2
LOADU=${LOADU:-r1cpu-load}
CONDS=${CONDS:-A B C}
read -ra CL <<< "$CONDS"
BINS=${BINS:-/root/rcpu/bins.txt}
mkdir -p "$OUT"
order() { local k=$(( ($1 - 1) % ${#CL[@]} )); echo "${CL[@]:k}" "${CL[@]:0:k}"; }
# have OUTC CELL: the ok runs of CELL (BIN= aside) in OUTC/res.jsonl
have() {
  python3 -c 'import json, sys
cell = " ".join(t for t in sys.argv[2].split() if not t.startswith("BIN="))
n = 0
try:
    for l in open(sys.argv[1]):
        r = json.loads(l)
        if r.get("ok") and " ".join(t for t in r.get("cell", "").split() if not t.startswith("BIN=")) == cell:
            n += 1
except OSError:
    pass
print(n)' "$1/res.jsonl" "$2"
}
for rep in "$@"; do
  while read -r cell; do
    case "$cell" in ''|'#'*) continue ;; esac
    n=$(printf '%s' "$cell" | md5sum | cut -c1-8)
    printf '%s\n' "$cell" > "$OUT/plan-$n.txt"
    for c in $(order "$rep"); do
      # DEADLINE (epoch s): no run starts that could end after it (blocks <= 25 min)
      if [ -n "${DEADLINE:-}" ] && [ $(( $(date +%s) + ${RUN_S:-60} )) -gt "$DEADLINE" ]; then
        echo "$(date +%T) deadline: rep $rep cell $n cond $c not run" >> "$OUT/ab.log"
        continue
      fi
      case $c in A|D) systemctl freeze "$LOADU.scope" ;; *) systemctl thaw "$LOADU.scope" ;; esac
      k=$(( $(have "$OUT/$c" "$cell") + 1 ))
      fs=$(systemctl show "$LOADU.scope" -p FreezerState --value 2>/dev/null)
      echo "$(date +%T) rep $rep cell $n cond $c load $fs run $k start" >> "$OUT/ab.log"
      if [ "$c" = B ] || [ "$c" = D ]; then
        RIG_CPUS=${RIG_CPUS:-0-5} REPS=$k LOAD_MAX=1000 BINS=$BINS "$D/run.sh" "$OUT/plan-$n.txt" "$OUT/$c"
      else
        REPS=$k LOAD_MAX=1000 BINS=$BINS "$D/run.sh" "$OUT/plan-$n.txt" "$OUT/$c"
      fi
      echo "$(date +%T) rep $rep cell $n cond $c done rc $?" >> "$OUT/ab.log"
    done
  done < "$CELLS"
done
systemctl freeze "$LOADU.scope" 2>/dev/null
echo "$(date +%T) end (load frozen)" >> "$OUT/ab.log"

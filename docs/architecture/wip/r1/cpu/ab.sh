#!/bin/bash
# ab.sh CELLS OUT REP... — the A/B of the CPU split (R1 speed), inside one
# rig-lock block. For each repeat REP and each cell (one PLAN line a line of
# CELLS) it runs the cell once in each condition, in a rotating order
# (repeat 1: A B C, 2: B C A, 3: C A B, 4: A C B, 5: B A C):
#   A  quiet host, today's rule: the load frozen, the rig unpinned
#   B  the rig pinned to RIG_CPUS (0-5) and the load running on the test cores
#   C  the rig unpinned and the load running (agents that do not pin yet)
# CONDS="A B" limits the conditions. The load is the scope LOADU
# (r1cpu-load.scope, load.sh through r1-cpu.sh), frozen and thawed here with
# systemctl. Each condition has its own run.sh OUT (OUT/A, OUT/B, OUT/C);
# REPS=REP makes run.sh run just that repeat (the earlier ones are there).
# LOAD_MAX=1000: the 1-minute load average lags the freeze by minutes, so
# the quiet runs are checked by what each CPU did in the measuring window
# instead (the "cpus" field, ana.py compose).
set -u
D=${D:-/root/hs2-wt/r1-cpu/docs/architecture/wip/rig-scale}
CELLS=${1:?usage: ab.sh CELLS OUT REP...}; OUT=${2:?}; shift 2
LOADU=${LOADU:-r1cpu-load}
CONDS=${CONDS:-A B C}
BINS=${BINS:-/root/rcpu/bins.txt}
mkdir -p "$OUT"
order() { case $1 in 1) echo A B C ;; 2) echo B C A ;; 3) echo C A B ;; 4) echo A C B ;; *) echo B A C ;; esac; }
for rep in "$@"; do
  n=0
  while read -r cell; do
    case "$cell" in ''|'#'*) continue ;; esac
    n=$((n + 1))
    printf '%s\n' "$cell" > "$OUT/plan-$n.txt"
    for c in $(order "$rep"); do
      case " $CONDS " in *" $c "*) ;; *) continue ;; esac
      # DEADLINE (epoch s): no run starts that could end after it (blocks <= 25 min)
      if [ -n "${DEADLINE:-}" ] && [ $(( $(date +%s) + ${RUN_S:-60} )) -gt "$DEADLINE" ]; then
        echo "$(date +%T) deadline: rep $rep cell $n cond $c not run" >> "$OUT/ab.log"
        continue
      fi
      if [ "$c" = A ]; then systemctl freeze "$LOADU.scope"; else systemctl thaw "$LOADU.scope"; fi
      fs=$(systemctl show "$LOADU.scope" -p FreezerState --value 2>/dev/null)
      echo "$(date +%T) rep $rep cell $n cond $c load $fs start" >> "$OUT/ab.log"
      if [ "$c" = B ]; then
        RIG_CPUS=${RIG_CPUS:-0-5} REPS=$rep LOAD_MAX=1000 BINS=$BINS "$D/run.sh" "$OUT/plan-$n.txt" "$OUT/$c"
      else
        REPS=$rep LOAD_MAX=1000 BINS=$BINS "$D/run.sh" "$OUT/plan-$n.txt" "$OUT/$c"
      fi
      echo "$(date +%T) rep $rep cell $n cond $c done rc $?" >> "$OUT/ab.log"
    done
  done < "$CELLS"
done
systemctl freeze "$LOADU.scope" 2>/dev/null
echo "$(date +%T) end (load frozen)" >> "$OUT/ab.log"

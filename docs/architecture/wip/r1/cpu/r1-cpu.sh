#!/bin/bash
# r1-cpu.sh CMD [ARGS...] — run a test, build or fuzz command on the test
# cores (R1 speed; docs/architecture/wip/r1/cpu/REPORT.fa.md), installed as
# /root/r1-cpu.sh. The command runs in a transient systemd scope (cgroup v2,
# system.slice) that holds only it and its children:
#   AllowedCPUs=TEST_CPUS (6-11)   it and every child stay on these CPUs (a
#                                  taskset inside cannot leave them); Go's
#                                  GOMAXPROCS follows (6)
#   CPUWeight=CPU_WEIGHT (20)      under the production services (100) when
#                                  they share a CPU; CPU_WEIGHT=idle makes the
#                                  scope an idle group (cpu.idle: any other
#                                  task that wakes takes the CPU at once)
#   nice 19, I/O class idle
# Nothing outside the scope changes. The rig blocks run on the other cores
# (run.sh RIG_CPUS=0-5). Environment, working directory and exit status are
# the command's own. R1CPU_UNIT=NAME names the scope NAME.scope (systemctl
# freeze/thaw/stop NAME.scope acts on exactly these processes). Without
# systemd (R1CPU_NOSCOPE=1, or systemd-run failing its probe) it falls back
# to taskset + nice + ionice (no weight).
set -u
[ $# -gt 0 ] || { echo "usage: r1-cpu.sh CMD [ARGS...]" >&2; exit 2; }
CPUS=${TEST_CPUS:-6-11}
W=${CPU_WEIGHT:-20}
props=(-p "AllowedCPUs=$CPUS" -p "CPUWeight=$W" -p IOWeight=10)
if [ -z "${R1CPU_NOSCOPE:-}" ] && systemd-run --scope --quiet --collect "${props[@]}" true 2>/dev/null; then
  unit=()
  [ -n "${R1CPU_UNIT:-}" ] && unit=(--unit="$R1CPU_UNIT")
  exec systemd-run --scope --quiet --collect "${unit[@]}" "${props[@]}" --nice=19 -- ionice -c3 "$@"
fi
exec taskset -c "$CPUS" nice -n 19 ionice -c3 "$@"

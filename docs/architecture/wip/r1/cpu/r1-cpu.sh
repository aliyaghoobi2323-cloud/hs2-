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
# the command's own. The scope is named r1cpu-PID.scope (R1CPU_UNIT=NAME:
# NAME.scope), so every test job on the host can be paused at once, e.g. for
# a field measurement, and let go after it:
#   systemctl freeze 'r1cpu-*.scope'   ...   systemctl thaw 'r1cpu-*.scope'
# Without systemd (R1CPU_NOSCOPE=1, or systemd-run failing its probe) it
# falls back to taskset + nice + ionice (no weight, no scope).
set -u
[ $# -gt 0 ] || { echo "usage: r1-cpu.sh CMD [ARGS...]" >&2; exit 2; }
CPUS=${TEST_CPUS:-6-11}
W=${CPU_WEIGHT:-20}
props=(-p "AllowedCPUs=$CPUS" -p "CPUWeight=$W" -p IOWeight=10)
if [ -z "${R1CPU_NOSCOPE:-}" ] && systemd-run --scope --quiet --collect "${props[@]}" true 2>/dev/null; then
  exec systemd-run --scope --quiet --collect --unit="${R1CPU_UNIT:-r1cpu-$$}" "${props[@]}" --nice=19 -- ionice -c3 "$@"
fi
exec taskset -c "$CPUS" nice -n 19 ionice -c3 "$@"

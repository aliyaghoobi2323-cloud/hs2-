#!/bin/bash
# rigguard.sh LOGDIR CMD [ARGS...]: run one rig block (CMD, for example
# run.sh PLAN OUT) with a record of this shared host's TCP memory (R1 round
# 4; the round-3 judge's rig pushed the host into TCP memory pressure six
# times, with 25 users of 16 connections on unfixed binaries).
#
# RIG_GUARD=log, the default (the owner's decision, 28 Oct: test users may
# see some slowness, and no test may be cut short or changed by a guard):
# CMD runs as it would without this script; memwatch only records, every
# RIG_MEM_EVERY s (0.5), the host's TCP memory (/proc/net/sockstat "TCP: ...
# mem P", pages, all namespaces) against net.ipv4.tcp_mem, and the root
# namespace's TcpExtTCPMemoryPressures (/proc/net/netstat), before, during
# and after the block. Each run of run.sh also records its own window
# (hostmem.json; ana.py marks the runs that saw pressure).
#
# RIG_GUARD=cap (opt-in, off by default): the block also runs in a transient
# systemd scope (cgroup v2) with MemoryMax RIG_MEM_MAX (1100M), no swap and
# OOMPolicy=continue (cgroup v2 charges the buffers of the scope's sockets to
# it), and memwatch aborts it (SIGTERM to the scope, SIGKILL 5 s later, then
# rig.sh down) when the host's TCP memory reaches RIG_MEM_ABORT pages
# (default 98 % of tcp_mem[0]) or the counter grows. That mode changes what
# a run measures (a capped rig, a cut block), so a block run so says it.
#
# LOGDIR/guard.log: the mode, tcp_mem, the samples' summary (host TCP max,
# TCPMemoryPressures at start and end, and in cap mode the scope's
# memory.peak, memory.events and ABORT with why). LOGDIR/memwatch.tsv: one
# line per sample (epoch, host TCP pages, and in cap mode the scope's socket
# and memory bytes). Exit status: CMD's (3 when aborted in cap mode).
set -u
LOGDIR=${1:?usage: rigguard.sh LOGDIR CMD [ARGS...]}; shift
[ $# -gt 0 ] || { echo "usage: rigguard.sh LOGDIR CMD [ARGS...]" >&2; exit 2; }
D=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$LOGDIR"; LOGDIR=$(cd "$LOGDIR" && pwd)
MODE=${RIG_GUARD:-log}
[ "$MODE" = cap ] || MODE=log
MAX=${RIG_MEM_MAX:-1100M}; EVERY=${RIG_MEM_EVERY:-0.5}
read -r TM0 TM1 TM2 < /proc/sys/net/ipv4/tcp_mem
ABORT=${RIG_MEM_ABORT:-$((TM0 * 98 / 100))}
b=$(basename "$LOGDIR"); b=${b//[^A-Za-z0-9_-]/_}
UNIT=rigguard-${b:0:40}-$$
GL=$LOGDIR/guard.log; MW=$LOGDIR/memwatch.tsv
glog() { echo "$(date +%T) $*" >> "$GL"; }

tcpmem() { awk '/^TCP:/{for (i = 1; i <= NF; i++) if ($i == "mem") print $(i + 1)}' /proc/net/sockstat; }
pressures() { awk '/^TcpExt:/{if (!h) {for (i = 2; i <= NF; i++) n[i] = $i; h = 1} else {for (i = 2; i <= NF; i++) if (n[i] == "TCPMemoryPressures") print $i}}' /proc/net/netstat; }

p0=$(pressures); m0=$(tcpmem)
export RIG_GUARD_MODE=$MODE
if [ "$MODE" = log ]; then
  glog "mode log (record only); tcp_mem $TM0 $TM1 $TM2; host TCP $m0 pages, TCPMemoryPressures $p0 at start; RIG_ID '${RIG_ID:-}'; cmd: $*"
  "$@" &
  SR=$!
  trap 'glog "stopped by a signal"; kill -TERM $SR 2>/dev/null; wait $SR; exit 143' TERM INT
else
  glog "mode cap: unit $UNIT.scope MemoryMax $MAX; tcp_mem $TM0 $TM1 $TM2; abort at $ABORT pages; host TCP $m0 pages, TCPMemoryPressures $p0 at start; RIG_ID '${RIG_ID:-}'; cmd: $*"
  if [ "${m0:-0}" -ge "$ABORT" ]; then
    glog "ABORT before start: the host's TCP memory is already at $m0 pages (abort at $ABORT)"
    exit 3
  fi
  export RIG_MEM_MAX=$MAX
  systemd-run --scope --quiet --collect --unit="$UNIT" -p MemoryMax="$MAX" -p MemorySwapMax=0 -p OOMPolicy=continue -- "$@" &
  SR=$!
fi
# cap mode: the scope's cgroup (it exists once systemd-run has started CMD)
CG=""
if [ "$MODE" = cap ]; then
  for _ in $(seq 1 50); do
    CG=$(systemctl show -p ControlGroup --value "$UNIT.scope" 2>/dev/null)
    [ -n "$CG" ] && [ -d "/sys/fs/cgroup$CG" ] && break
    kill -0 $SR 2>/dev/null || break
    sleep 0.1
  done
  [ -n "$CG" ] && CG=/sys/fs/cgroup$CG
  glog "cgroup ${CG:-none}"
  # stop: every process of the scope (TERM, then KILL 5 s later) and the
  # rig's namespaces; also when the guard itself is told to stop
  stopscope() {
    systemctl kill --signal=TERM "$UNIT.scope" 2>/dev/null
    for _ in $(seq 1 50); do kill -0 $SR 2>/dev/null || break; sleep 0.1; done
    systemctl kill --signal=KILL "$UNIT.scope" 2>/dev/null
    RIG_ID=${RIG_ID:-} "$D/rig.sh" down >> "$GL" 2>&1
  }
  trap 'glog "stopped by a signal: stopping $UNIT.scope"; stopscope; wait $SR; exit 143' TERM INT
fi

aborted=""; hi=0; sockhi=0; n=0; peak=""; ev=""; over0=0; over1=0
while kill -0 $SR 2>/dev/null; do
  m=$(tcpmem); p=$(pressures)
  sock=0; cur=0
  if [ -n "$CG" ] && [ -r "$CG/memory.stat" ]; then
    sock=$(awk '$1 == "sock" {print $2}' "$CG/memory.stat")
    cur=$(cat "$CG/memory.current" 2>/dev/null || echo 0)
    # kept from the latest sample: the cgroup goes with the scope
    x=$(cat "$CG/memory.peak" 2>/dev/null) && [ -n "$x" ] && peak=$x
    x=$(tr '\n' ' ' < "$CG/memory.events" 2>/dev/null) && [ -n "$x" ] && ev=$x
  fi
  echo "$(date +%s.%N | cut -c1-14) ${m:-0} ${sock:-0} ${cur:-0}" >> "$MW"
  n=$((n + 1)); [ "${m:-0}" -gt "$hi" ] && hi=$m; [ "${sock:-0}" -gt "$sockhi" ] && sockhi=$sock
  [ "${m:-0}" -ge "$TM0" ] && over0=$((over0 + 1))
  [ "${m:-0}" -ge "$TM1" ] && over1=$((over1 + 1))
  if [ "$MODE" = cap ]; then
    if [ "${m:-0}" -ge "$ABORT" ]; then
      aborted="host TCP memory $m pages >= $ABORT"
    elif [ -n "$p" ] && [ -n "$p0" ] && [ "$p" -gt "$p0" ]; then
      aborted="TcpExtTCPMemoryPressures grew $p0 -> $p"
    fi
    if [ -n "$aborted" ]; then
      glog "ABORT: $aborted; stopping $UNIT.scope"
      stopscope
      break
    fi
  fi
  sleep "$EVERY"
done
wait $SR; rc=$?
p1=$(pressures)
glog "end: mode $MODE, status $rc; $n samples; host TCP max $hi pages (start $m0; tcp_mem $TM0 $TM1 $TM2; samples at or past tcp_mem[0] $over0, past tcp_mem[1] (pressure) $over1); TCPMemoryPressures $p0 -> $p1$( [ "$MODE" = cap ] && echo "; scope socket max $sockhi bytes; memory.peak ${peak:-?}; memory.events ${ev:-?}")"
if [ -n "$aborted" ]; then
  exit 3
fi
exit $rc

#!/bin/bash
# rigguard.sh LOGDIR CMD [ARGS...]: run one rig block (CMD, for example
# run.sh PLAN OUT) so that it cannot push this shared host into TCP memory
# pressure (R1 round 4; the round-3 judge's rig did, six times, with 25 users
# of 16 connections on unfixed binaries: round3-judge.fa.md, priority 1).
#
#   1. a memory cap: CMD runs in a transient systemd scope (cgroup v2) with
#      MemoryMax RIG_MEM_MAX (1100M) and no swap. Cgroup v2 charges the
#      buffers of every socket the scope's processes make to the scope, and
#      a socket whose charge fails is held back (its own memcg pressure: its
#      windows shrink), so the rig's TCP memory stays under the cap whatever
#      a binary does. 1100M is under tcp_mem[0] of the Kharej server (279672
#      pages, 1.15 GB): past that mark Linux grows no socket's buffers,
#      production's included; past tcp_mem[1] (372896 pages) it shrinks every
#      socket's receive window (the round-3 collapse).
#   2. memwatch, every RIG_MEM_EVERY s (0.5): the host's TCP memory
#      (/proc/net/sockstat "TCP: ... mem P", pages, all namespaces) and the
#      root namespace's TcpExtTCPMemoryPressures (/proc/net/netstat). The
#      block is aborted (SIGTERM to every process of the scope, SIGKILL 5 s
#      later, then rig.sh down) when the TCP memory reaches RIG_MEM_ABORT
#      pages (default 98 % of tcp_mem[0]) or the counter grows.
#
# LOGDIR/guard.log: the scope, the limits, the samples' summary, the scope's
# memory.peak and memory.events (a "max" count above 0 means the cap held the
# rig back: the block's runs were limited by it, and are flagged so), and
# ABORT with why when it aborted. LOGDIR/memwatch.tsv: one line per sample
# (epoch, host TCP pages, the scope's socket bytes, its memory bytes).
# Exit status: CMD's, or 3 when aborted. RIG_GUARD=0 (run.sh) skips the
# guard; nothing here touches anything outside the scope and the rig's own
# namespaces (RIG_ID).
set -u
LOGDIR=${1:?usage: rigguard.sh LOGDIR CMD [ARGS...]}; shift
[ $# -gt 0 ] || { echo "usage: rigguard.sh LOGDIR CMD [ARGS...]" >&2; exit 2; }
D=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$LOGDIR"; LOGDIR=$(cd "$LOGDIR" && pwd)
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
glog "unit $UNIT.scope MemoryMax $MAX; tcp_mem $TM0 $TM1 $TM2; abort at $ABORT pages; host TCP $m0 pages, TCPMemoryPressures $p0 at start; RIG_ID '${RIG_ID:-}'; cmd: $*"
if [ "${m0:-0}" -ge "$ABORT" ]; then
  glog "ABORT before start: the host's TCP memory is already at $m0 pages (abort at $ABORT)"
  exit 3
fi

export RIG_MEM_MAX=$MAX
systemd-run --scope --quiet --collect --unit="$UNIT" -p MemoryMax="$MAX" -p MemorySwapMax=0 -- "$@" &
SR=$!
# the scope's cgroup (it exists once systemd-run has started CMD)
CG=""
for _ in $(seq 1 50); do
  CG=$(systemctl show -p ControlGroup --value "$UNIT.scope" 2>/dev/null)
  [ -n "$CG" ] && [ -d "/sys/fs/cgroup$CG" ] && break
  kill -0 $SR 2>/dev/null || break
  sleep 0.1
done
[ -n "$CG" ] && CG=/sys/fs/cgroup$CG
glog "cgroup ${CG:-none}"

aborted=""; hi=0; sockhi=0; n=0; peak=""; ev=""
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
  if [ "${m:-0}" -ge "$ABORT" ]; then
    aborted="host TCP memory $m pages >= $ABORT"
  elif [ -n "$p" ] && [ -n "$p0" ] && [ "$p" -gt "$p0" ]; then
    aborted="TcpExtTCPMemoryPressures grew $p0 -> $p"
  fi
  if [ -n "$aborted" ]; then
    glog "ABORT: $aborted; stopping $UNIT.scope"
    systemctl kill --signal=TERM "$UNIT.scope" 2>/dev/null
    for _ in $(seq 1 50); do kill -0 $SR 2>/dev/null || break; sleep 0.1; done
    systemctl kill --signal=KILL "$UNIT.scope" 2>/dev/null
    RIG_ID=${RIG_ID:-} "$D/rig.sh" down >> "$GL" 2>&1
    break
  fi
  sleep "$EVERY"
done
wait $SR; rc=$?
glog "end: status $rc; $n samples; host TCP max $hi pages (start $m0, abort $ABORT); scope socket max $sockhi bytes; memory.peak ${peak:-?}; memory.events ${ev:-?}; TCPMemoryPressures $(pressures)"
if [ -n "$aborted" ]; then
  exit 3
fi
exit $rc

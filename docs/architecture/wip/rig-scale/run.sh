#!/bin/bash
# run.sh PLAN OUT: the big rig's runner (R1 task 2). One cell per PLAN line:
#
#   N=16 RATE=50 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=s5 BIN=old [KEY=VAL ...]
#
#   N      hs2 links (min_links = max_links = N on both servers)
#   RATE   bottleneck, Mbit/s, each direction      RTT  ms      LOSS  %, each direction
#          (RATE_UP=R: the upload direction at R instead, an asymmetric path)
#   MODE   fqcodel | pfifo:LIMIT | agg:RATE | police:RATE | police:RATE+LEAF   (rig.sh)
#   USERS  synthetic users = source addresses (<= 500 IPv4, + 1 with V6, + TUN users)
#   MIX    s5 | lone16 | one_vs_16 | newheavy | tunping     (ana.py spec)
#          | home | gamer_vs_dl | stream_vs_dl   (the user classes: game,
#          gameudp, call, stream, web next to bulk users; keys GAME=tcp|udp|both,
#          SBR, CALLR, PAGE, UPN, TAIL; a cell with gameudp turns on the edge's
#          "udp": true, UDP on the user ports)
#   BIN    old | new: the binaries in $OLD_BIN / $NEW_BIN
#   extra: DIR=dn|up (bulk direction of lone16/one_vs_16/newheavy), DN, UP (s5
#          flows, 4 and 2), TDN, TUNDIR (tunping's bulk through hs0, 4 dn),
#          CONNS (the heavy user's connections, 16), NHBULK (newheavy's existing
#          bulk user, 1), V6=K (port user K is one IPv6 /64, a fresh address
#          per connection), TUN=K (the first K users go through the TUN, each
#          from its own inner address; tunping: 1), TUNV6=1 (those inner
#          addresses and hs0 in IPv6), WARM, DUR, SETTLE (s), AGGQ_MS,
#          POLICE_BURST_MS, and any HS2_*=V, passed to both hs2 processes (the
#          R1 kill switches, for example).
#          KILLQ=T, KILLWHAT=quiet|bulk|any (R1 round-4 judge): one link of
#          the edge is reset T s after the client starts (killlink; the
#          client logs its connections and opens cut ones again, ana.py
#          "kill"); MIX frozen (FROZEN, FWAVE, FREAD, FRCVBUF, FROZENU):
#          downloads whose app stops reading next to probes.
#          KILLREP=K, KILLGAP=S (R1 red team, second pass): K kills, one
#          every S s from KILLQ on (kill-K.json each; kill.json the first);
#          KILLWHAT=all resets every link socket of the edge at once.
#          PROCSTAT=1: each hs2's RSS, threads and open files every second
#          (procstat-edge.tsv, procstat-exit.tsv in the raw dir); with
#          HS2_GOSTAT=1 each hs2 also logs its goroutines and heap a second.
#
# PROC_TRACE=S (environment, R1 merge step 5): every S s from the links being
# up to the end of the run, both hs2 processes' open fds, VmRSS (kB),
# threads and goroutines (HS2_PPROF on loopback inside each namespace) go to
# raw/ID/proc-edge.tsv and proc-exit.tsv ("epoch fds rss_kb threads
# goroutines"); the run then stays up POST_IDLE s (20) after the client ends,
# so what the users' connections held can be seen to go (a leak stays), and
# status-*-idle.txt/json are taken at its end.
#
# Lines that differ only in BIN form one cell; the cell runs in the order
# ABBABA (A = old) for REPS (3) repeats each; a cell with one BIN runs REPS
# times. Each run: fresh namespaces (rig.sh up), fresh cert and shared key (in
# $WORK, never in the repo), two hs2 processes (edge in r-edge listening, exit
# in r-exit dialing: reverse l3mtcp), the rigload server in r-exit and its
# client in r-edge, then everything torn down, also on failure or Ctrl-C. One
# JSON line per run goes to OUT/res.jsonl (ana.py compose; "ok": false with
# "errs" when the run failed); raw files to OUT/raw/ID/. Resumable: a run whose
# id already has an ok line is skipped. Before each run it waits (up to
# LOAD_WAIT s, 120) while the 1-minute load is over LOAD_MAX (8): other work
# shares this host.
set -u
D=$(cd "$(dirname "$0")" && pwd)
PLAN=${1:?usage: run.sh PLAN OUT}; OUT=${2:?usage: run.sh PLAN OUT}
mkdir -p "$OUT/raw"; OUT=$(cd "$OUT" && pwd)
# the host's TCP memory (R1 round 4, rigguard.sh): by default (RIG_GUARD=log)
# the block is recorded only (tcp_mem, the TCP memory and the pressure
# counter before, during and after it), nothing capped or cut (the owner's
# decision); RIG_GUARD=cap (opt-in) runs it in a cgroup scope under a memory
# cap and aborts it near the pressure mark; RIG_GUARD=0 runs without either
if [ "${RIG_GUARD:-log}" != 0 ] && [ -z "${RIG_GUARDED:-}" ]; then
  RIG_GUARDED=1 exec "$D/rigguard.sh" "$OUT/guard-$(date +%Y%m%dT%H%M%S)" "$D/run.sh" "$PLAN" "$OUT"
fi
RES=$OUT/res.jsonl; LOG=$OUT/run.log; touch "$RES"
REPS=${REPS:-3}; LOAD_MAX=${LOAD_MAX:-8}; LOAD_WAIT=${LOAD_WAIT:-120}
OLD_BIN=${OLD_BIN:-/root/r1-bins/hs2-old}; NEW_BIN=${NEW_BIN:-}
export RIG_ID=${RIG_ID:-}
WORK=${WORK:-/tmp/rig-scale$RIG_ID}
E=r-edge$RIG_ID; X=r-exit$RIG_ID
PIDS=()
CUR=""   # the run in progress: its work dir (keys, configs)

log() { echo "$(date +%T) $*" >> "$LOG"; }

cleanup() {
  local p s
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in "${PIDS[@]}"; do kill -9 "$p" 2>/dev/null; done
  PIDS=()
  "$D/rig.sh" down
  if [ -n "$CUR" ]; then
    # the status and warm files hs2 keeps for these two configs
    s=/run/hs2/$(echo "${CUR#/}" | tr / -)
    rm -f "$s-edge.json.status.json" "$s-exit.json.status.json" "$s-edge.json.warm" "$s-exit.json.warm"
    rm -rf "$CUR"
    CUR=""
  fi
}
trap 'cleanup; exit 130' INT TERM
trap 'cleanup; rmdir "$WORK" 2>/dev/null' EXIT

# rigload, built once per OUT (its own module, standard library only)
RL=$OUT/rigload
if [ ! -x "$RL" ] || [ -n "$(find "$D/rigload" -name '*.go' -newer "$RL")" ]; then
  (cd "$D/rigload" && CGO_ENABLED=0 nice -n 10 /usr/local/go/bin/go build -o "$RL" .) || { echo "rigload build failed" >&2; exit 1; }
fi

# BINS=FILE (the judge, R1 task 17): more binaries than old and new, one
# per line "LABEL PATH [HS2_K=V ...]"; the HS2_* go to both hs2 processes of
# that label's runs, so one cell can interleave binaries and their variants
BINS=${BINS:-}
binpath() {
  case "$1" in
    old) echo "$OLD_BIN" ;;
    new) echo "$NEW_BIN" ;;
    *) [ -n "$BINS" ] && awk -v l="$1" '$1 == l {print $2; exit}' "$BINS" ;;
  esac
}
binenv() {
  [ -n "$BINS" ] && awk -v l="$1" '$1 == l {for (i = 3; i <= NF; i++) if ($i ~ /^HS2_[A-Z0-9_]+=[A-Za-z0-9_.:+-]*$/) printf " %s", $i; exit}' "$BINS"
}

# hs2 config: edge (mode dial, reverse: listens) and exit (mode listen, reverse:
# dials), as the field's reverse l3mtcp tunnel; keys and cert made per run
mkconf() {
  local w=$1 n=$2 udp=${3:-0} key uj=""
  [ "$udp" = 1 ] && uj='"udp":true,'
  key=$(openssl rand -hex 32)
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$w/key.pem" -out "$w/cert.pem" -days 2 -subj /CN=rig.example.com 2>/dev/null || return 1
  cat > "$w/edge.json" <<J
{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"10.201.0.1:2096",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1320,
 "shared_key":"$key","cert_file":"$w/cert.pem","key_file":"$w/key.pem","backend_addr":"builtin",
 "forward_ports":"5311,5312",$uj"min_links":$n,"max_links":$n,"per_link":8}
J
  cat > "$w/exit.json" <<J
{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"10.201.0.1:2096","sni":"rig.example.com",
 "bind_local_ip":"10.202.0.1","iface":"hs0","local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1320,
 "shared_key":"$key","expose":"127.0.0.1:5211","port_map":"5311=127.0.0.1:5211,5312=127.0.0.1:5212",
 "min_links":$n,"max_links":$n,"per_link":8}
J
  chmod 600 "$w"/*.json "$w/key.pem"
}

# ss snapshot of the link sockets in both namespaces, and the host's CPU counters
sssnap() {
  local tag=$1 r=$2
  ip netns exec $X ss -tinH state established '( dport = :2096 )' > "$r/ss-exit-$tag.txt" 2>/dev/null
  ip netns exec $E ss -tinH state established '( sport = :2096 )' > "$r/ss-edge-$tag.txt" 2>/dev/null
  head -1 /proc/stat > "$r/stat-$tag.txt"
}

# proctrace R: the PROC_TRACE sampler (runs in the background; it uses
# run_one's pe and px)
proctrace() {
  local r=$1 side p ns g
  while :; do
    for side in edge exit; do
      if [ $side = edge ]; then p=$pe; ns=$E; else p=$px; ns=$X; fi
      [ -r "/proc/$p/status" ] || continue
      g=$(ip netns exec $ns curl -s -m 2 "http://127.0.0.1:6061/debug/pprof/goroutine?debug=1" 2>/dev/null | awk '/^goroutine profile: total/{print $NF; exit}')
      echo "$(date +%s.%N) $(ls "/proc/$p/fd" 2>/dev/null | wc -l) $(awk '/^VmRSS:/{print $2}' "/proc/$p/status") $(awk '/^Threads:/{print $2}' "/proc/$p/status") ${g:-NA}" >> "$r/proc-$side.tsv"
    done
    sleep "$PROC_TRACE"
  done
}

# killlink R WHAT C0 (KILLQ, R1 round-4 judge): reset one link socket of the
# edge with ss -K (SOCK_DESTROY: the edge's socket fails, the exit gets a
# reset, as from a DPI box) — the busiest one (bytes acked + received) of
# class WHAT: quiet (the QUIET links' sockets run cubic) or bulk (bbr); any:
# the busiest of all; a pool without QUIET links (production) falls back to
# any. kill.json: what, the link's class and peer, the classes' counts, how
# many sockets ss killed, and when (s after the client's start C0, and the
# hs2 log's clock).
killlink() {
  local r=$1 what=$2 c0=$3 k=${4:-1} t clk pick cls nq nb killed sfx=""
  [ "$k" -gt 1 ] && sfx="-$k"
  t=$(date +%s.%N); clk=$(date +%H:%M:%S.%N | cut -c1-15)
  ip netns exec $E ss -tinH state established '( sport = :2096 )' > "$r/ss-edge-kill$sfx.txt" 2>/dev/null
  if [ "$what" = all ]; then
    read -r nq nb < <(awk '/^[0-9]/ {next} {if ($0 ~ / cubic /) q++; else b++} END {printf "%d %d\n", q, b}' "$r/ss-edge-kill$sfx.txt")
    killed=$(ip netns exec $E ss -K -tnH state established '( sport = :2096 )' 2>&1 | grep -c '^[0-9]')
    echo "{\"what\":\"all\",\"class\":\"all\",\"peer\":\"all\",\"n_quiet\":${nq:-0},\"n_bulk\":${nb:-0},\"killed\":${killed:-0},\"k\":$k,\"t_rel\":$(awk -v a="$c0" -v b="$t" 'BEGIN{printf "%.3f", b - a}'),\"clock\":\"$clk\"}" > "$r/kill$sfx.json"
    return
  fi
  read -r pick cls nq nb < <(awk -v want="$what" '
    /^[0-9]/ { peer = $4; next }
    peer != "" {
      cc = ($0 ~ / cubic /) ? "quiet" : "bulk"
      b = 0
      if (match($0, /bytes_acked:[0-9]+/)) b += substr($0, RSTART + 12, RLENGTH - 12)
      if (match($0, /bytes_received:[0-9]+/)) b += substr($0, RSTART + 15, RLENGTH - 15)
      n[cc]++
      if ((want == "any" || cc == want) && (bp == "" || b > best)) { best = b; bp = peer; bc = cc }
      if (ap == "" || b > anyb) { anyb = b; ap = peer; ac = cc }
      peer = ""
    }
    END {
      if (bp == "") { bp = ap; bc = ac }
      printf "%s %s %d %d\n", (bp == "" ? "none" : bp), (bc == "" ? "none" : bc), n["quiet"], n["bulk"]
    }' "$r/ss-edge-kill$sfx.txt")
  killed=0
  if [ "$pick" != none ]; then
    killed=$(ip netns exec $E ss -K -tnH state established "( sport = :2096 and dst $pick )" 2>&1 | grep -c '^[0-9]')
  fi
  echo "{\"what\":\"$what\",\"class\":\"$cls\",\"peer\":\"$pick\",\"n_quiet\":${nq:-0},\"n_bulk\":${nb:-0},\"killed\":${killed:-0},\"k\":$k,\"t_rel\":$(awk -v a="$c0" -v b="$t" 'BEGIN{printf "%.3f", b - a}'),\"clock\":\"$clk\"}" > "$r/kill$sfx.json"
}

# this process's cgroup (v2), and a counter of its memory.events (0: none;
# RIG_GUARD=cap's)
memcg_dir() { echo "/sys/fs/cgroup$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup)"; }
memcg_ev() {
  local f; f="$(memcg_dir)/memory.events"
  if [ -r "$f" ]; then awk -v k="$1" '$1 == k {v = $2} END {print v + 0}' "$f"; else echo 0; fi
}
# the host's TCP memory (pages, /proc/net/sockstat) and the root namespace's
# TcpExtTCPMemoryPressures (/proc/net/netstat)
host_tcpmem() { awk '/^TCP:/{for (i = 1; i <= NF; i++) if ($i == "mem") print $(i + 1)}' /proc/net/sockstat; }
host_pressures() { awk '/^TcpExt:/{if (!h) {for (i = 2; i <= NF; i++) n[i] = $i; h = 1} else {for (i = 2; i <= NF; i++) if (n[i] == "TCPMemoryPressures") print $i + 0}}' /proc/net/netstat; }

run_one() {
  local id=$1 bin=$2 order=$3 rep=$4 cell=$5
  local r=$OUT/raw/$id w=$WORK/$id bp benv tok k v hs2env="" wait=0 ok=1 why=""
  local total="" nusers="" ntun="" pe="" px="" hup="" htop=""
  # the cell's variables (validated: KEY=VAL, nothing a shell would expand)
  local N RATE RTT LOSS MODE USERS MIX WARM DUR SETTLE AGGQ_MS POLICE_BURST_MS V6 RATE_UP PANEL_SNDBUF KILLQ KILLWHAT KILLREP KILLGAP PROCSTAT
  WARM=5; DUR=30; SETTLE=5; V6=0; AGGQ_MS=50; POLICE_BURST_MS=20; RATE_UP=; KILLQ=; KILLWHAT=quiet; KILLREP=1; KILLGAP=5; PROCSTAT=0
  # the rigload server's (the panel's) send buffer a connection, bytes (R1
  # round 4, opt-in: autotuned, the default 0, it holds ~5 MB a connection
  # on loopback, 1.2 GB for 400 downloads; a bound changes what the
  # many-connections cells measure)
  PANEL_SNDBUF=0
  for tok in $cell; do
    k=${tok%%=*}; v=${tok#*=}
    [[ "$k" =~ ^[A-Z][A-Z0-9_]*$ && "$v" =~ ^[A-Za-z0-9_.:+-]*$ ]] || { log "bad token $tok"; return 1; }
    case "$k" in
      HS2_*) hs2env="$hs2env $k=$v" ;;
      N|RATE|RTT|LOSS|MODE|USERS|MIX|WARM|DUR|SETTLE|AGGQ_MS|POLICE_BURST_MS|V6|RATE_UP|PANEL_SNDBUF|KILLQ|KILLWHAT|KILLREP|KILLGAP|PROCSTAT) printf -v "$k" '%s' "$v" ;;
    esac
  done
  bp=$(binpath "$bin")
  benv=$(binenv "$bin")
  hs2env="$hs2env$benv"
  rm -rf "$r"; mkdir -p "$r" "$w"; CUR=$w
  # quiet host
  while [ $wait -lt "$LOAD_WAIT" ] && awk -v m="$LOAD_MAX" '{exit !($1 > m)}' /proc/loadavg; do sleep 10; wait=$((wait + 10)); done
  # the host at the start of the run: uptime and the five busiest processes
  # (%CPU over 0.5 s, command name only)
  hup=$(uptime | sed 's/^ *//' | tr -d '"\\')
  htop=$(top -bn2 -d0.5 -w 200 2>/dev/null | awk '/^top -/{f++} f==2 && /^ *[0-9]+ /{print $9" "$12}' | head -5 | paste -sd';' | tr -d '"\\')
  echo "{\"id\":\"$id\",\"bin\":\"$bin\",\"bin_path\":\"$bp\",\"bin_sha\":\"$( [ -f "$bp" ] && sha256sum "$bp" | cut -c1-12)\",\"bin_env\":\"${benv# }\",\"order\":\"$order\",\"rep\":$rep,\"cell\":\"$cell\",\"load0\":$(cut -d' ' -f1 /proc/loadavg),\"waited_s\":$wait,\"start\":\"$(date -Is)\",\"edge_cpus\":\"${EDGE_CPUS:-}\",\"exit_cpus\":\"${EXIT_CPUS:-}\",\"panel_sndbuf\":${PANEL_SNDBUF:-0},\"uptime\":\"$hup\",\"top\":\"$htop\"}" > "$r/meta.json"
  log "start $id ($cell BIN=$bin) load $(cut -d' ' -f1 /proc/loadavg) waited ${wait}s"
  local mx0 ok0 hp0 hm0 hsp
  mx0=$(memcg_ev max); ok0=$(memcg_ev oom_kill)
  # the host's TCP memory through the run (each 0.5 s, the most and the
  # samples past tcp_mem[0] and tcp_mem[1]) and the pressure counter: the
  # tables mark a run that saw pressure
  hp0=$(host_pressures); hm0=$(host_tcpmem)
  ( read -r a b c < /proc/sys/net/ipv4/tcp_mem; hi=0; n0=0; n1=0
    trap 'echo "$hi $n0 $n1" > "$r/hostmem.max"; exit 0' TERM
    while :; do
      m=$(host_tcpmem); m=${m:-0}
      [ "$m" -gt "$hi" ] && hi=$m
      [ "$m" -ge "$a" ] && n0=$((n0 + 1))
      [ "$m" -ge "$b" ] && n1=$((n1 + 1))
      echo "$hi $n0 $n1" > "$r/hostmem.max"
      sleep 0.5
    done ) &
  hsp=$!; PIDS+=($hsp)
  run_steps || ok=0
  kill "$hsp" 2>/dev/null; wait "$hsp" 2>/dev/null
  read -r hhi hn0 hn1 < "$r/hostmem.max" 2>/dev/null
  echo "{\"tcp_mem\":\"$(tr '\t' ' ' < /proc/sys/net/ipv4/tcp_mem)\",\"start_pages\":${hm0:-0},\"max_pages\":${hhi:-0},\"samples_past_tcp_mem0\":${hn0:-0},\"samples_past_tcp_mem1\":${hn1:-0},\"pressures\":$(( $(host_pressures) - ${hp0:-0} ))}" > "$r/hostmem.json"
  if [ "${RIG_GUARD_MODE:-}" = cap ]; then
    # the opt-in cap during this run: "max" events (the cap held the rig's
    # memory back: this run was limited by it), OOM kills, the scope's peak
    echo "{\"max_events\":$(( $(memcg_ev max) - mx0 )),\"oom_kills\":$(( $(memcg_ev oom_kill) - ok0 )),\"peak\":$(cat "$(memcg_dir)/memory.peak" 2>/dev/null || echo 0),\"cap\":\"${RIG_MEM_MAX:-}\"}" > "$r/memcg.json"
  fi
  ip netns exec r-mid$RIG_ID tc -s qdisc show dev me > "$r/tc-dn.txt" 2>&1
  ip netns exec r-mid$RIG_ID tc -s qdisc show dev mx > "$r/tc-up.txt" 2>&1
  cleanup
  python3 "$D/ana.py" compose "$r" "$ok" "$why" >> "$RES" || log "FAIL $id: compose"
  log "done $id ok=$ok $why"
}

# run_steps: the body of one run (it uses run_one's variables); on failure it
# sets why and returns 1, and run_one still tears down and writes the line
run_steps() {
  local n t0 c0 pc lw te="" tx="" udp
  [ -x "$bp" ] || { why="no binary for BIN=$bin ($bp)"; return 1; }
  # optional CPU sets (the field edge has 4 cores): EDGE_CPUS=0-3 EXIT_CPUS=4-11
  [ -n "${EDGE_CPUS:-}" ] && te="taskset -c $EDGE_CPUS"
  [ -n "${EXIT_CPUS:-}" ] && tx="taskset -c $EXIT_CPUS"
  # the users and the timeline
  read -r nusers total ntun < <(python3 "$D/ana.py" spec "$cell" "$r/spec.json" 2> "$r/spec.err")
  [ -n "$ntun" ] || { why="spec: $(tail -1 "$r/spec.err")"; return 1; }
  RATE=$RATE RATE_UP=${RATE_UP:-$RATE} RTT=$RTT LOSS=$LOSS MODE=$MODE NUSERS=$nusers NTUN=$ntun V6=$V6 AGGQ_MS=$AGGQ_MS \
    "$D/rig.sh" up 2> "$r/rig.err" || { why="rig up: $(tail -1 "$r/rig.err")"; return 1; }
  # gameudp in the cell: UDP on the edge's user ports ("udp": true)
  udp=0; grep -qE '"kind": "(gameudp|udpflood)"' "$r/spec.json" && udp=1
  mkconf "$w" "$N" $udp || { why="hs2 config"; return 1; }
  # PROC_TRACE: goroutines through Go's pprof, on loopback in each namespace
  [ "${PROC_TRACE:-0}" != 0 ] && hs2env="$hs2env HS2_PPROF=127.0.0.1:6061"
  # the server keeps each game and call flow's record in srv/ (the client reads it)
  ip netns exec $X "$RL" server -listen 127.0.0.1:5211,127.0.0.1:5212,:5213 -report "$r/srv" -sndbuf "$PANEL_SNDBUF" > "$r/rigload-server.log" 2>&1 &
  PIDS+=($!)
  ip netns exec $E $te env HS2_NO_TUNE=1 $hs2env "$bp" run -c "$w/edge.json" > "$r/edge.log" 2>&1 &
  pe=$!; PIDS+=($pe)
  sleep 0.5
  ip netns exec $X $tx env HS2_NO_TUNE=1 $hs2env "$bp" run -c "$w/exit.json" > "$r/exit.log" 2>&1 &
  px=$!; PIDS+=($px)
  t0=$(date +%s.%N)
  lw=$((30 + N / 5))
  n=$("$D/rig.sh" wait-links "$N" $lw)
  echo "{\"links_wait_s\":$(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.2f", b - a}'),\"links_at_start\":${n:-0},\"pid_edge\":$pe,\"pid_exit\":$px}" > "$r/links.json"
  [ "${n:-0}" -ge "$N" ] || { why="only ${n:-0} of $N links up after $lw s"; return 1; }
  "$D/rig.sh" tun-up 2> "$r/tun-up.err" || { why="tun-up: $(tail -1 "$r/tun-up.err")"; return 1; }
  if [ "${PROC_TRACE:-0}" != 0 ]; then
    proctrace "$r" &
    PIDS+=($!)
  fi
  case "$MODE" in
    *police:*) MODE=$MODE POLICE_BURST_MS=$POLICE_BURST_MS "$D/rig.sh" police > "$r/police.log" 2>&1 &
               PIDS+=($!); sleep 1.5 ;;
  esac
  # TRACE_SS=1 (the judge of round 2): ss -ti of the link sockets of both
  # ends every 0.5 s through the run (pacing and max_pacing_rate, delivery,
  # srtt, min_rtt), each sample after a "T <epoch>" line: the trace of α's cap
  if [ "${TRACE_SS:-0}" = 1 ]; then
    ( while :; do
        echo "T $(date +%s.%N)" >> "$r/sstrace-exit.txt"
        ip netns exec $X ss -tinH state established '( dport = :2096 )' >> "$r/sstrace-exit.txt" 2>/dev/null
        echo "T $(date +%s.%N)" >> "$r/sstrace-edge.txt"
        ip netns exec $E ss -tinH state established '( sport = :2096 )' >> "$r/sstrace-edge.txt" 2>/dev/null
        sleep 0.5
      done ) &
    PIDS+=($!)
  fi
  # measurement: the client from t=0 to total; snapshots at WARM and near the end
  ip netns exec $E timeout $(awk -v t="$total" 'BEGIN{printf "%d", t + 30}') "$RL" client -spec "$r/spec.json" -srvdir "$r/srv" > "$r/client.json" 2> "$r/client.err" &
  pc=$!; PIDS+=($pc)
  c0=$(date +%s.%N)
  echo "$c0" > "$r/client-t0.txt"
  # KILLQ=T (R1 round-4 judge, the red team's C4): one link of the edge is
  # reset T s after the client starts (killlink)
  if [ -n "$KILLQ" ]; then
    # KILLREP kills, KILLGAP s apart (the red team's second pass: flapping)
    ( for kk in $(seq 1 "$KILLREP"); do
        sleep "$(awk -v c="$c0" -v k="$KILLQ" -v g="$KILLGAP" -v i="$kk" -v n="$(date +%s.%N)" 'BEGIN{s = c + k + (i - 1) * g - n; if (s < 0) s = 0; printf "%.3f", s}')"
        killlink "$r" "$KILLWHAT" "$c0" "$kk"
      done ) > "$r/kill.log" 2>&1 &
    PIDS+=($!)
  fi
  # PROCSTAT=1: each hs2's RSS (kB), threads and open files every second
  if [ "$PROCSTAT" = 1 ]; then
    ( while :; do
        for pp in "edge $pe" "exit $px"; do
          set -- $pp
          [ -r "/proc/$2/status" ] || continue
          echo "$(date +%s.%N | cut -c1-14) $(awk '/^VmRSS:/{r=$2} /^Threads:/{t=$2} END{printf "%d %d", r, t}' /proc/$2/status) $(ls /proc/$2/fd 2>/dev/null | wc -l)" >> "$r/procstat-$1.tsv"
        done
        sleep 1
      done ) &
    PIDS+=($!)
  fi
  sleep "$WARM"
  sssnap start "$r"
  LC_ALL=C pidstat -u -p "$pe,$px" 1 $(awk -v t="$total" -v w="$WARM" 'BEGIN{printf "%d", t - w - 1}') > "$r/pidstat.txt" 2>&1 &
  PIDS+=($!)
  sleep $(awk -v t="$total" -v c="$c0" -v n="$(date +%s.%N)" 'BEGIN{s = c + t - 1.2 - n; if (s < 0) s = 0; printf "%.2f", s}')
  "$bp" status -c "$w/edge.json" > "$r/status-edge.txt" 2>&1
  "$bp" status -c "$w/exit.json" > "$r/status-exit.txt" 2>&1
  cp "/run/hs2/$(echo "${w#/}" | tr / -)-edge.json.status.json" "$r/status-edge.json" 2>/dev/null
  cp "/run/hs2/$(echo "${w#/}" | tr / -)-exit.json.status.json" "$r/status-exit.json" 2>/dev/null
  sssnap end "$r"
  grep -E "^Vm(RSS|HWM):" /proc/$pe/status > "$r/mem-edge.txt" 2>/dev/null
  grep -E "^Vm(RSS|HWM):" /proc/$px/status > "$r/mem-exit.txt" 2>/dev/null
  local cst=0
  wait "$pc" || cst=$?
  date +%s.%N > "$r/client-end.txt"
  [ "$cst" = 0 ] || { why="client exit status $cst ($(tail -1 "$r/client.err"))"; return 1; }
  if [ "${PROC_TRACE:-0}" != 0 ]; then
    # the users' connections are gone: what they held must go too
    sleep "${POST_IDLE:-20}"
    "$bp" status -c "$w/edge.json" > "$r/status-edge-idle.txt" 2>&1
    "$bp" status -c "$w/exit.json" > "$r/status-exit-idle.txt" 2>&1
    cp "/run/hs2/$(echo "${w#/}" | tr / -)-edge.json.status.json" "$r/status-edge-idle.json" 2>/dev/null
    cp "/run/hs2/$(echo "${w#/}" | tr / -)-exit.json.status.json" "$r/status-exit-idle.json" 2>/dev/null
    ip netns exec $E ss -tinH state established "( sport = :2096 )" > "$r/ss-edge-idle.txt" 2>/dev/null
  fi
  kill -0 "$pe" 2>/dev/null || { why="edge hs2 exited during the run"; return 1; }
  kill -0 "$px" 2>/dev/null || { why="exit hs2 exited during the run"; return 1; }
  return 0
}

# the schedule: id bin order rep cell (tab separated); CR stripped first
tr -d '\r' < "$PLAN" > "$OUT/plan.txt"
python3 "$D/ana.py" schedule "$OUT/plan.txt" "$REPS" > "$OUT/schedule.tsv" || exit 1
log "plan $PLAN: $(wc -l < "$OUT/schedule.tsv") runs"
while IFS=$'\t' read -r id bin order rep cell; do
  grep -q "\"id\": *\"$id\".*\"ok\": *true" "$RES" && continue
  run_one "$id" "$bin" "$order" "$rep" "$cell" < /dev/null
done < "$OUT/schedule.tsv"
log "done $PLAN"

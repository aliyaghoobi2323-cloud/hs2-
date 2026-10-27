#!/bin/bash
# run.sh PLAN OUT: the big rig's runner (R1 task 2). One cell per PLAN line:
#
#   N=16 RATE=50 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=s5 BIN=old [KEY=VAL ...]
#
#   N      hs2 links (min_links = max_links = N on both servers)
#   RATE   bottleneck, Mbit/s, each direction      RTT  ms      LOSS  %, each direction
#   MODE   fqcodel | pfifo:LIMIT | agg:RATE | police:RATE | police:RATE+LEAF   (rig.sh)
#   USERS  synthetic users = source addresses (<= 500 IPv4, + 1 with V6, + TUN users)
#   MIX    s5 | lone16 | one_vs_16 | newheavy | tunping     (ana.py spec)
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
if [ ! -x "$RL" ] || [ "$D/rigload/main.go" -nt "$RL" ]; then
  (cd "$D/rigload" && CGO_ENABLED=0 nice -n 10 /usr/local/go/bin/go build -o "$RL" .) || { echo "rigload build failed" >&2; exit 1; }
fi

# BINS=FILE (the judge, R1 task 17): more binaries than old and new, one
# per line "LABEL PATH [HS2_K=V ...]"; the HS2_* go to both hs2 processes of
# that label's runs, so one cell can interleave binaries and their variants
BINS=${BINS:-}
binpath() {
  case "$1" in
    raw) echo raw ;;  # lab (r1-diag): no hs2, the client straight to the server
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
  local w=$1 n=$2 key
  key=$(openssl rand -hex 32)
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$w/key.pem" -out "$w/cert.pem" -days 2 -subj /CN=rig.example.com 2>/dev/null || return 1
  cat > "$w/edge.json" <<J
{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"10.201.0.1:2096",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1320,
 "shared_key":"$key","cert_file":"$w/cert.pem","key_file":"$w/key.pem","backend_addr":"builtin",
 "forward_ports":"5311,5312","min_links":$n,"max_links":$n,"per_link":8}
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
  date +%s.%N > "$r/sstime-$tag.txt"
  ip netns exec $X ss -tinH state established '( dport = :2096 )' > "$r/ss-exit-$tag.txt" 2>/dev/null
  ip netns exec $E ss -tinH state established '( sport = :2096 )' > "$r/ss-edge-$tag.txt" 2>/dev/null
  head -1 /proc/stat > "$r/stat-$tag.txt"
}

run_one() {
  local id=$1 bin=$2 order=$3 rep=$4 cell=$5
  local r=$OUT/raw/$id w=$WORK/$id bp benv tok k v hs2env="" wait=0 ok=1 why=""
  local total="" nusers="" ntun="" pe="" px="" hup="" htop=""
  # the cell's variables (validated: KEY=VAL, nothing a shell would expand)
  local N RATE RTT LOSS MODE USERS MIX WARM DUR SETTLE AGGQ_MS POLICE_BURST_MS V6
  WARM=5; DUR=30; SETTLE=5; V6=0; AGGQ_MS=50; POLICE_BURST_MS=20
  for tok in $cell; do
    k=${tok%%=*}; v=${tok#*=}
    [[ "$k" =~ ^[A-Z][A-Z0-9_]*$ && "$v" =~ ^[A-Za-z0-9_.:+-]*$ ]] || { log "bad token $tok"; return 1; }
    case "$k" in
      HS2_*) hs2env="$hs2env $k=$v" ;;
      N|RATE|RTT|LOSS|MODE|USERS|MIX|WARM|DUR|SETTLE|AGGQ_MS|POLICE_BURST_MS|V6) printf -v "$k" '%s' "$v" ;;
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
  echo "{\"id\":\"$id\",\"bin\":\"$bin\",\"bin_path\":\"$bp\",\"bin_sha\":\"$( [ -f "$bp" ] && sha256sum "$bp" | cut -c1-12)\",\"bin_env\":\"${benv# }\",\"order\":\"$order\",\"rep\":$rep,\"cell\":\"$cell\",\"load0\":$(cut -d' ' -f1 /proc/loadavg),\"waited_s\":$wait,\"start\":\"$(date -Is)\",\"edge_cpus\":\"${EDGE_CPUS:-}\",\"exit_cpus\":\"${EXIT_CPUS:-}\",\"uptime\":\"$hup\",\"top\":\"$htop\"}" > "$r/meta.json"
  log "start $id ($cell BIN=$bin) load $(cut -d' ' -f1 /proc/loadavg) waited ${wait}s"
  run_steps || ok=0
  labstop "$r"
  ip netns exec r-mid$RIG_ID tc -s qdisc show dev me > "$r/tc-dn.txt" 2>&1
  ip netns exec r-mid$RIG_ID tc -s qdisc show dev mx > "$r/tc-up.txt" 2>&1
  cleanup
  python3 "$D/ana.py" compose "$r" "$ok" "$why" >> "$RES" || log "FAIL $id: compose"
  log "done $id ok=$ok $why"
}

# lab (r1-diag, round 2 step 1): CAP=1 tcpdump of the link's packets
# (headers only) on the edge's and the exit's veth, from before the link's
# SYN; TCS=1 the bottleneck qdiscs every 100 ms. labstop ends both (and
# gzips the captures) before the namespaces go.
LABP=()
labstart() {
  local r=$1
  LABP=()
  if [ "${CAP:-0}" = 1 ]; then
    ip netns exec $E tcpdump -i e0 -s 80 -B 32768 -nn -w "$r/e0.pcap" tcp port 2096 2> "$r/cap-e0.err" &
    LABP+=($!)
    ip netns exec $X tcpdump -i x0 -s 80 -B 32768 -nn -w "$r/x0.pcap" tcp port 2096 2> "$r/cap-x0.err" &
    LABP+=($!)
    sleep 0.5
  fi
  if [ "${TCS:-0}" = 1 ]; then
    ( while :; do echo "T $(date +%s.%N)"; ip netns exec r-mid$RIG_ID tc -s qdisc show dev me | sed 's/^/D /'; ip netns exec r-mid$RIG_ID tc -s qdisc show dev mx | sed 's/^/U /'; sleep 0.1; done ) > "$r/tcs.txt" 2>/dev/null &
    LABP+=($!)
  fi
}
labstop() {
  local p
  for p in "${LABP[@]}"; do kill -INT "$p" 2>/dev/null; done
  for p in "${LABP[@]}"; do wait "$p" 2>/dev/null; done
  LABP=()
}

# run_steps: the body of one run (it uses run_one's variables); on failure it
# sets why and returns 1, and run_one still tears down and writes the line
run_steps() {
  local n t0 c0 pc lw te="" tx=""
  [ "$bp" = raw ] || [ -x "$bp" ] || { why="no binary for BIN=$bin ($bp)"; return 1; }
  # optional CPU sets (the field edge has 4 cores): EDGE_CPUS=0-3 EXIT_CPUS=4-11
  [ -n "${EDGE_CPUS:-}" ] && te="taskset -c $EDGE_CPUS"
  [ -n "${EXIT_CPUS:-}" ] && tx="taskset -c $EXIT_CPUS"
  # the users and the timeline
  read -r nusers total ntun < <(python3 "$D/ana.py" spec "$cell" "$r/spec.json" 2> "$r/spec.err")
  [ -n "$ntun" ] || { why="spec: $(tail -1 "$r/spec.err")"; return 1; }
  RATE=$RATE RTT=$RTT LOSS=$LOSS MODE=$MODE NUSERS=$nusers NTUN=$ntun V6=$V6 AGGQ_MS=$AGGQ_MS \
    "$D/rig.sh" up 2> "$r/rig.err" || { why="rig up: $(tail -1 "$r/rig.err")"; return 1; }
  mkconf "$w" "$N" || { why="hs2 config"; return 1; }
  ip netns exec $X "$RL" server -listen 127.0.0.1:5211,127.0.0.1:5212,:5213 > "$r/rigload-server.log" 2>&1 &
  PIDS+=($!)
  labstart "$r"
  local rlenv=""
  if [ "${TRACE:-0}" = 1 ]; then
    mkdir -p "$r/trace"
    hs2env="$hs2env HS2_LAB_TRACE=$r/trace"
    rlenv="RIGLOAD_PROBE_DUMP=$r/probes.json"
  fi
  if [ "$bp" = raw ]; then
    # lab: no hs2; every flow straight to the rigload server in r-exit
    python3 -c 'import json,sys; p=sys.argv[1]; s=json.load(open(p)); s["bulk"]=s["echo"]="10.202.0.1:5213"; [u.update(src="") for u in s["users"] if u.get("path","port")=="port"]; s["users"]=[u for u in s["users"] if u.get("path","port")=="port"]; json.dump(s,open(p,"w"))' "$r/spec.json"
    echo '{"links_wait_s":0,"links_at_start":0,"raw":true}' > "$r/links.json"
    pe=$$; px=$$
  else
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
  fi
  case "$MODE" in
    *police:*) MODE=$MODE POLICE_BURST_MS=$POLICE_BURST_MS "$D/rig.sh" police > "$r/police.log" 2>&1 &
               PIDS+=($!); sleep 1.5 ;;
  esac
  # measurement: the client from t=0 to total; snapshots at WARM and near the end
  ip netns exec $E timeout $(awk -v t="$total" 'BEGIN{printf "%d", t + 30}') env $rlenv "$RL" client -spec "$r/spec.json" > "$r/client.json" 2> "$r/client.err" &
  pc=$!; PIDS+=($pc)
  c0=$(date +%s.%N)
  sleep "$WARM"
  sssnap start "$r"
  LC_ALL=C pidstat -u -p "$pe,$px" 1 $(awk -v t="$total" -v w="$WARM" 'BEGIN{printf "%d", t - w - 1}') > "$r/pidstat.txt" 2>&1 &
  PIDS+=($!)
  sleep $(awk -v t="$total" -v c="$c0" -v n="$(date +%s.%N)" 'BEGIN{s = c + t - 1.2 - n; if (s < 0) s = 0; printf "%.2f", s}')
  if [ "$bp" != raw ]; then
  "$bp" status -c "$w/edge.json" > "$r/status-edge.txt" 2>&1
  "$bp" status -c "$w/exit.json" > "$r/status-exit.txt" 2>&1
  fi
  cp "/run/hs2/$(echo "${w#/}" | tr / -)-edge.json.status.json" "$r/status-edge.json" 2>/dev/null
  cp "/run/hs2/$(echo "${w#/}" | tr / -)-exit.json.status.json" "$r/status-exit.json" 2>/dev/null
  sssnap end "$r"
  grep -E "^Vm(RSS|HWM):" /proc/$pe/status > "$r/mem-edge.txt" 2>/dev/null
  grep -E "^Vm(RSS|HWM):" /proc/$px/status > "$r/mem-exit.txt" 2>/dev/null
  wait "$pc" || { why="client exit status $? ($(tail -1 "$r/client.err"))"; return 1; }
  [ "$bp" = raw ] && return 0
  kill -0 "$pe" 2>/dev/null || { why="edge hs2 exited during the run"; return 1; }
  kill -0 "$px" 2>/dev/null || { why="exit hs2 exited during the run"; return 1; }
  return 0
}

# the schedule: id bin order rep cell (tab separated); CR stripped first
tr -d '\r' < "$PLAN" > "$OUT/plan.txt"
if [ -n "${SCHED:-}" ]; then
  cp "$SCHED" "$OUT/schedule.tsv"  # lab (r1-diag): a schedule made elsewhere, as ana.py schedule prints one
else
  python3 "$D/ana.py" schedule "$OUT/plan.txt" "$REPS" > "$OUT/schedule.tsv" || exit 1
fi
log "plan $PLAN: $(wc -l < "$OUT/schedule.tsv") runs"
while IFS=$'\t' read -r id bin order rep cell; do
  grep -q "\"id\": *\"$id\".*\"ok\": *true" "$RES" && continue
  run_one "$id" "$bin" "$order" "$rep" "$cell" < /dev/null
done < "$OUT/schedule.tsv"
log "done $PLAN"

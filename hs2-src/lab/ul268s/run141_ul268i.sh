#!/usr/bin/env bash
# One hs2 lab experiment: iran <-> [netem] <-> kharej in network namespaces,
# real hs2 binaries on each side, probe traffic through the forwarded port.
# Prints one JSON line. Needs root, iproute2, ethtool, openssl.
#
#   BIN=./hs2 MODE=mtcp RATE=50mbit DELAY=40ms LOSS=0.005 lab/run.sh
#
# Env (defaults in brackets):
#   BIN [./hs2] (BIN_KH: kharej binary if different)  MODE [mtcp]  RATE [50mbit]  DELAY [40ms]  QUEUE [500ms]
#   LOSS [0]  FLOWRATE [0]  BULK [8]  UP [0]  T [20s]  UDP [0]
#   MIN_LINKS [4]  MAX_LINKS [16]
#   IRAN_EXTRA / KHAREJ_EXTRA: extra JSON fields, e.g. '"per_link":4,'
#   KEEP=1 leaves the namespaces and logs in place.
#   KH_QDISC / IR_QDISC: a qdisc on the Kharej / Iran side's link into the path, e.g. "cake bandwidth 92mbit".
#   NETEM_EXTRA: extra emulator flags, e.g. "-burstloss 0.3 -badms 30 -goodms 2000" (bursty loss).
#   PROBE_EXTRA: extra probe flags, e.g. "-seg 4000000" (reconnect per 4 MB download).
#   CALL [] e.g. 2mbit: a two-way video-call-like stream (probe -call)   GAME [0] 1 = game-like stream
#   UCALL [] e.g. 2mbit: a two-way UDP call (probe -ucall, WebRTC-like) through hs0 to the kharej
#     probe server at 10.77.0.2:5201, while the rest goes as VIA says (needs a TUN mode)
#   LATE [0] e.g. 5s: echo/call/game and the hs0 ping start this long after the downloads
#   CROWD [0] e.g. 300: light connections (probe -crowd) so the pool grows to tens of links;
#     CROWD_EVERY [700ms] mean interval of their requests. Raise MAX_LINKS with it (e.g. 128).
#   The hs0 ping (modes with a TUN, e.g. l3mtcp) is `ping -i 0.2` to the other end's inner
#   address; its per-reply times give ping_p50/p90/p99 and ping_over120_pct (ms, replies only).
#   WEB [0] 1 = page loads (probe -web): every 2-3 s, 4 parallel short connections of 50-500 KB.
#   VIA [] tun = all probe traffic (downloads, echo, call, game, web, crowd) goes to the kharej
#     probe server at the other end's inner TUN address 10.77.0.2:5201 through hs0 instead of the
#     forwarded port 127.0.0.1:8443 (needs a TUN mode, e.g. l3mtcp). The JSON then also has "via",
#     "tun_route" (`ip route get` in the Iran namespace) and the Iran hs0 byte counters over the
#     probe run (tun_rx_mb / tun_tx_mb).
#   JOURNEY [0] e.g. 20: virtual users (probe -journey) each doing reels -> video call -> big
#     download -> app pings -> game in turn, starts spread over 10 s; JSON "journey" aggregates
#     every phase over all users. JOURNEY_EXTRA: its flags, e.g. "-journeyscale 0.5 -jreels 3".
#     Combines with BULK (set BULK=0 for the journey alone), CROWD, CALL, GAME, WEB.
#   KILL_AT [0] e.g. 20: that many seconds into the probe, reset KILL_PCT [75] percent of the
#     links (lab/killrebuild.py) and time the pool's rebuild; JSON "kill" has the counts, the
#     seconds to 50/90/100% of the links (rebuild_t*_s) and the count over time.
#   REVERSE [0] 1 = the tunnel in reverse on the one path (kharej dials iran's 192.168.50.1:2096);
#     PATHS=2 is always reverse.
#   NOWARM [0] 1 = delete the Iran side's warm file (last link target) first, so every run
#     starts from the same pool size.
#   LOGDIR [] keep both sides' logs there as RUNTAG-ir.log / RUNTAG-kh.log.
#   LAB_ID=n runs in namespaces irn/khn/midn so experiments can run in parallel.
#   HS2_TUNE_* variables are passed through to hs2 (see cmd/hs2 applyTuning).
set -u
D=/opt/hs2lab/bs201/src/hs2-src/lab
ID=${LAB_ID:-}
W=${WORKDIR:-/tmp/hs2lab}$ID
IR=ir$ID; KH=kh$ID; MID=mid$ID
# interface names carry the id too: veths are created in the root namespace
# first, so parallel runs must not share names
VI=vi$ID; VK=vk$ID; MA=xa$ID; MB=xb$ID
BIN=${BIN:-./hs2}; BIN_KH=${BIN_KH:-$BIN}; MODE=${MODE:-mtcp}
RATE=${RATE:-50mbit}; DELAY=${DELAY:-40ms}; QUEUE=${QUEUE:-500ms}; LOSS=${LOSS:-0}
FLOWRATE=${FLOWRATE:-0}; BULK=${BULK:-8}; UP=${UP:-0}; T=${T:-20s}; UDP=${UDP:-0}
IRAN_EXTRA=${IRAN_EXTRA:-}; KHAREJ_EXTRA=${KHAREJ_EXTRA:-}
VIA=${VIA:-}; WEB=${WEB:-0}
# PATHS=2: a second Iran<->kharej IP pair (192.168.51.0/24) with its own netem
# (PATH_A_NETEM / PATH_B_NETEM: extra netem flags per path), and the tunnel in
# reverse with multipath lb on; LB_WEIGHTS="A,B" are the Iran IPs' weights.
PATHS=${PATHS:-1}; PATH_A_NETEM=${PATH_A_NETEM:-}; PATH_B_NETEM=${PATH_B_NETEM:-}; LB_WEIGHTS=${LB_WEIGHTS:-50,50}
# LB_MODE=fixed|auto is lb.mode; auto runs with HS2_LB_FAST=1 (quality tick
# 5 s, rebalance 10 s instead of 30 s / 60 s) so a 60 s run sees it adapt.
LB_MODE=${LB_MODE:-fixed}
# LB_DIRECT=1 (with PATHS=2): the tunnel in DIRECT mode — kharej listens on
# 192.168.50.2 and 192.168.51.2 (LB_WEIGHTS), iran dials from 192.168.50.1.
# LB_INIT=one: the listening side starts with its primary address only.
# LB_EVENTS="SEC:SPEC ..." (stage 6): at SEC seconds into the probe, rewrite
# the LISTENING side's own lb list and send it SIGHUP (no restart); SPEC is
# w=A,B (both addresses, these weights) or one (the primary only). The
# dialing side learns the change from the listening side's announcement.
LB_DIRECT=${LB_DIRECT:-0}; LB_INIT=${LB_INIT:-}; LB_EVENTS=${LB_EVENTS:-}
if [ "$LB_MODE" = auto ]; then export HS2_LB_FAST=${HS2_LB_FAST:-1}; fi
JOURNEY=${JOURNEY:-0}; JOURNEY_EXTRA=${JOURNEY_EXTRA:-}
CALL=${CALL:-}; GAME=${GAME:-0}; LATE=${LATE:-0}; CROWD=${CROWD:-0}; CROWD_EVERY=${CROWD_EVERY:-700ms}
# --- multi-gigabit lab (see lab/LAB3G.md) ---
# EMU [go] the path emulator: go = lab/netem (userspace AF_PACKET bridge, tops
#   out near ~1 Gbit/s, see LAB3G.md); kernel = the kernel's sch_netem on the
#   middle namespace's veths (tc mirred bridges them, netem does rate+delay+loss
#   per direction), accurate to 3 Gbit/s and beyond. With kernel, NETEM_EXTRA /
#   PATH_*_NETEM are extra `tc qdisc ... netem` arguments (tc syntax), and the
#   ends keep software GSO + GRO (TSO off) so frames in the middle stay MTU-size
#   and loss is per frame, as on a real path.
# NSTUNE [1 with EMU=kernel, else 0] the Iran/kharej namespaces get the TCP
#   sysctls hs2's own tuning sets on a real server ("high" profile: tcp_rmem /
#   tcp_wmem max 32 MB, bbr), which a 3 Gbit/s x 80 ms path needs.
# TUNNEL [1] 0 = no hs2 and no probe: the bare path, for checking the emulator
#   (idle ping RTT + the HEAVY iperf3 straight between 192.168.50.1 and .2).
# HEAVY [] down|up: one heavy user, an iperf3 (HEAVY_P [1] parallel streams,
#   HEAVY_SECS [T-1] s, first HEAVY_OMIT [3] s left out) through the tunnel's
#   user port 8444 (kharej port_map 8444 -> 127.0.0.1:5202), alongside whatever
#   the probe runs (light users: BULK=0 CALL=2mbit GAME=1 ...). down = kharej
#   -> Iran (iperf3 -R). JSON "heavy" has Mbit/s, per-second min/p50 and the CPU
#   of both hs2 processes (cores, and the busiest thread) over the measured part.
# PPROF [0] 1 = both hs2 serve pprof on 127.0.0.1:6060 in their namespace and a
#   CPU profile of each is taken over the heavy run (LOGDIR/RUNTAG-{ir,kh}.prof).
# IR_ENV / KH_ENV: "VAR=val ..." given to one side's hs2 only.
EMU=${EMU:-go}; TUNNEL=${TUNNEL:-1}; HEAVY=${HEAVY:-}; HEAVY_P=${HEAVY_P:-1}
HEAVY_OMIT=${HEAVY_OMIT:-3}; PPROF=${PPROF:-0}; IR_ENV=${IR_ENV:-}; KH_ENV=${KH_ENV:-}
HEAVY_SECS=${HEAVY_SECS:-$(( ${T%s} - 1 ))}
NSTUNE=${NSTUNE:-$([ "$EMU" = kernel ] && echo 1 || echo 0)}

# offl NS DEV end|mid: offloads. The go emulator reads and writes raw frames,
# so everything is off everywhere. With the kernel emulator the ends cut big
# sends to MTU frames in software (GSO, as a NIC's TSO would) and the middle
# keeps frames as they are, so netem drops single frames.
#
# GRO stays OFF on every veth: on a veth GRO switches on NAPI, whose receive
# ring is 256 frames, and netem's bursts at 3 Gbit/s overflowed it — 264k frames
# (5.8%) dropped silently on the middle's egress (`ip -s link`: TX dropped),
# 8x the configured loss. Without NAPI a frame goes to a CPU backlog
# (netdev_max_backlog, raised while EMU=kernel runs, see emu_backlog), and RPS
# hashes each flow to one CPU so a flow's frames stay in order.
offl(){
  if [ "$EMU" = kernel ]; then
    if [ "$3" = end ]; then ip netns exec $1 ethtool -K $2 tso off gso on gro off tx on sg on >/dev/null 2>&1
    else ip netns exec $1 ethtool -K $2 gro off tso off gso off >/dev/null 2>&1; fi
    ip netns exec $1 sh -c "printf %x $(( (1 << $(nproc --all)) - 1 )) > /sys/class/net/$2/queues/rx-0/rps_cpus" 2>/dev/null
  else
    ip netns exec $1 ethtool -K $2 tso off gso off gro off tx off rx off >/dev/null 2>&1
  fi
}
# knetem LOSS EXTRA: the `netem ...` arguments for the kernel emulator. The
# packet limit covers the propagation delay plus QUEUE of full frames at RATE,
# so the tail drop starts where the go emulator's queue bound does.
knetem(){
  python3 - "$RATE" "$DELAY" "$QUEUE" "$1" <<'PY'
import re, sys
def rate(s):
    m = re.match(r'([0-9.]+)\s*([kmg]?)(bit)?', s.lower()); v = float(m.group(1))
    return v * {'': 1, 'k': 1e3, 'm': 1e6, 'g': 1e9}[m.group(2)]
def dur(s):
    m = re.match(r'([0-9.]+)\s*(us|ms|s)?', s.lower()); v = float(m.group(1))
    return v * {'us': 1e-6, 'ms': 1e-3, 's': 1, None: 1}[m.group(2)]
r, d, q, l = rate(sys.argv[1]), dur(sys.argv[2]), dur(sys.argv[3]), float(sys.argv[4])
lim = max(1000, int(r * (d + q) / 8 / 1514) + 64) if r else 1000000
a = 'limit %d delay %dus' % (lim, round(d * 1e6))
if l > 0: a += ' loss random %.6f%%' % (l * 100)
if r: a += ' rate %dbit' % r
print(a)
PY
}
# emu_up K MA MB EXTRA: start the emulator for path K between MA (Iran side)
# and MB (kharej side) in the middle namespace.
emu_up(){
  if [ "$EMU" = kernel ]; then
    local k=$1 ma=$2 mb=$3 x=$4 p from to
    for p in "$ma $mb" "$mb $ma"; do
      read -r from to <<< "$p"
      ip netns exec $MID tc qdisc replace dev $from clsact
      ip netns exec $MID tc filter add dev $from ingress matchall action mirred egress redirect dev $to
      # this direction's netem sits on the egress of the far device
      ip netns exec $MID tc qdisc replace dev $to root netem $(knetem "$LOSS") $x || exit 1
    done
    echo "$ma $mb $x" > "$W/emu-$k"
  else
    ip netns exec $MID "$W/netem" -a $2 -b $3 -rate "$RATE" -delay "$DELAY" -queue "$QUEUE" \
      -loss "$LOSS" -flowrate "$FLOWRATE" -lossfile "$W/loss-$1" ${NETEM_EXTRA:-} $4 2>"$W/netem-$1.log" &
    echo $! > "$W/netem-$1.pid"; cat "$W/netem-$1.pid" >> "$W/pids"
  fi
}
# emu_loss K LOSS: change path K's random loss mid-run ("-" = back to LOSS).
emu_loss(){
  local l=$2; [ "$l" = - ] && l=$LOSS
  if [ "$EMU" = kernel ]; then
    read -r a b x < "$W/emu-$1"
    for dv in $a $b; do ip netns exec $MID tc qdisc change dev $dv root netem $(knetem "$l") $x; done
  else
    echo "$2" > "$W/loss-$1"; kill -USR1 "$(cat "$W/netem-$1.pid")"
  fi
}
# emu_stats: per direction, what the kernel emulator sent and dropped
# ("ab" = Iran -> kharej, "ba" = kharej -> Iran) as JSON fields.
emu_stats(){
  [ "$EMU" = kernel ] || return 0
  read -r a b x < "$W/emu-0" || return 0
  for p in "ab $b" "ba $a"; do
    set -- $p
    local vd=$(ip netns exec $MID cat /sys/class/net/$2/statistics/tx_dropped 2>/dev/null || echo 0)
    ip netns exec $MID tc -s qdisc show dev $2 | python3 -c '
import re, sys
t = sys.stdin.read(); m = re.search(r"Sent (\d+) bytes (\d+) pkt \(dropped (\d+), overlimits (\d+)", t)
s, d = int(m.group(2)), int(m.group(3))
print("\"emu_%s_pkts\":%d,\"emu_%s_drops\":%d,\"emu_%s_drop_pct\":%.3f,\"emu_%s_vethdrop\":%s," % (sys.argv[1], s, sys.argv[1], d, sys.argv[1], 100.0 * d / max(1, s + d), sys.argv[1], sys.argv[2]), end="")' $1 $vd
  done
}
# emu_backlog up|down: the per-CPU receive backlog (a host-wide sysctl, not
# per namespace) is raised from the default 1000 to 30000 frames while an
# EMU=kernel run is up, so netem's bursts are not dropped at the receiving
# veth, and put back at cleanup.
emu_backlog(){
  local f=/run/lab3g-backlog.old
  if [ "$1" = up ]; then
    [ -f $f ] || sysctl -n net.core.netdev_max_backlog > $f
    sysctl -qw net.core.netdev_max_backlog=30000
  elif [ -f $f ]; then
    sysctl -qw net.core.netdev_max_backlog="$(cat $f)"; rm -f $f
  fi
}
nstune(){
  [ "$NSTUNE" = 1 ] || return 0
  for n in $IR $KH; do
    ip netns exec $n sysctl -qw net.ipv4.tcp_rmem="4096 131072 33554432" net.ipv4.tcp_wmem="4096 65536 33554432" \
      net.ipv4.tcp_congestion_control=bbr net.ipv4.tcp_no_metrics_save=1 >/dev/null
  done
}
# heavy_go ADDR PORT: the heavy user's iperf3 in the background (-> $W/heavy.json)
# and the CPU of both hs2 over its measured part (-> $W/heavycpu.json).
heavy_go(){
  local R=""; [ "$HEAVY" = down ] && R="-R"
  rm -f "$W/heavy.json" "$W/heavycpu.json" "$W"/*.prof
  { sleep ${HEAVY_DELAY:-0}; date +%s.%N > "$W/hstart"; ip netns exec $IR iperf3 -c $1 -p $2 -P "$HEAVY_P" -t "$HEAVY_SECS" -O "$HEAVY_OMIT" ${HEAVY_XARGS:-} $R -J; } \
    > "$W/heavy.json" 2> "$W/heavy.err" & HVP=$!
  python3 "$D/cpusamp.py" "$HEAVY_OMIT" "$((HEAVY_SECS - 1))" "ir=${IRPID:-0}" "kh=${KHPID:-0}" > "$W/heavycpu.json" 2>/dev/null & HCP=$!
  if [ "$PPROF" = 1 ] && [ "$TUNNEL" = 1 ]; then
    local ps=$((HEAVY_SECS - HEAVY_OMIT - 2)); [ $ps -lt 3 ] && ps=3
    (sleep "$HEAVY_OMIT"; ip netns exec $KH curl -s -o "$W/kh.prof" "http://127.0.0.1:6060/debug/pprof/profile?seconds=$ps") & HPP1=$!
    (sleep "$HEAVY_OMIT"; ip netns exec $IR curl -s -o "$W/ir.prof" "http://127.0.0.1:6060/debug/pprof/profile?seconds=$ps") & HPP2=$!
    (sleep "$HEAVY_OMIT"; for k in mutex block; do ip netns exec $KH curl -s -o "$W/kh.$k.prof" "http://127.0.0.1:6060/debug/pprof/$k?seconds=$ps" & ip netns exec $IR curl -s -o "$W/ir.$k.prof" "http://127.0.0.1:6060/debug/pprof/$k?seconds=$ps" & done; wait) & HPP3=$!
  fi
}
# heavy_wait: in the main shell (a $(...) subshell cannot wait for these)
heavy_wait(){
  [ -n "${HVP:-}" ] || return 0
  wait "$HVP" "$HCP" ${HPP1:-} ${HPP2:-} ${HPP3:-} 2>/dev/null
}
# heavy_json: the "heavy":{...}, field once the iperf3 is done
heavy_json(){
  [ -n "${HVP:-}" ] || return 0
  if [ -n "${LOGDIR:-}" ]; then
    mkdir -p "$LOGDIR"
    [ -s "$W/heavy.json" ] && cp "$W/heavy.json" "$LOGDIR/${RUNTAG:-run}-heavy.json"; for s in ir kh ir.mutex kh.mutex ir.block kh.block; do [ -s "$W/$s.prof" ] && cp "$W/$s.prof" "$LOGDIR/${RUNTAG:-run}-$s.prof"; done
  fi
  python3 - "$W/heavy.json" "$W/heavycpu.json" "$HEAVY" "$HEAVY_P" "$W/heavy.err" <<'PY'
import json, sys
out = {"dir": sys.argv[3], "streams": int(sys.argv[4])}
try:
    j = json.load(open(sys.argv[1]))
    if "error" in j: out["error"] = j["error"]
    e = j.get("end", {})
    out["mbps"] = round(e.get("sum_received", {}).get("bits_per_second", 0) / 1e6, 1)
    out["sent_mbps"] = round(e.get("sum_sent", {}).get("bits_per_second", 0) / 1e6, 1)
    if "retransmits" in e.get("sum_sent", {}): out["retrans"] = e["sum_sent"]["retransmits"]
    iv = sorted(i["sum"]["bits_per_second"] / 1e6 for i in j.get("intervals", []) if not i["sum"].get("omitted"))
    if iv:
        out["int_min"] = round(iv[0], 1); out["int_p50"] = round(iv[len(iv) // 2], 1); out["int_max"] = round(iv[-1], 1)
except Exception as ex:
    out["error"] = (str(ex) + " " + open(sys.argv[5]).read())[-300:]
try: out["cpu"] = json.load(open(sys.argv[2]))
except Exception: pass
print('"heavy":%s,' % json.dumps(out), end="")
PY
}

cleanup(){
  for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in $(cat "$W"/pids 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  ip netns del $IR 2>/dev/null; ip netns del $KH 2>/dev/null; ip netns del $MID 2>/dev/null
  [ "$EMU" = kernel ] && emu_backlog down
}
mkdir -p "$W"; cleanup; rm -f "$W"/pids "$W"/*.log
mkdir -p "$W"
[ -x "$W/netem" ] || (cd "$D/.." && go build -o "$W/netem" ./lab/netem) || exit 1
[ -x "$W/probe" ] || (cd "$D/.." && go build -o "$W/probe" ./lab/probe) || exit 1
[ -f "$W/cert.pem" ] || openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/key.pem" \
  -out "$W/cert.pem" -days 30 -subj /CN=lab.example.com 2>/dev/null
cp "$BIN" "$W/hs2"; cp "$BIN_KH" "$W/hs2kh"

ip netns add $IR; ip netns add $KH; ip netns add $MID
ip link add $VI netns $IR type veth peer name $MA netns $MID
ip link add $VK netns $KH type veth peer name $MB netns $MID
ip -n $IR addr add 192.168.50.1/24 dev $VI
ip -n $KH addr add 192.168.50.2/24 dev $VK
for n in $IR $KH $MID; do ip -n $n link set lo up; done
for x in "$IR $VI end" "$KH $VK end" "$MID $MA mid" "$MID $MB mid"; do
  set -- $x
  offl $1 $2 $3
  ip -n $1 link set dev $2 up
done
[ "$EMU" = kernel ] && emu_backlog up
nstune
[ -n "${KH_QDISC:-}" ] && ip netns exec $KH tc qdisc replace dev $VK root ${KH_QDISC}
[ -n "${IR_QDISC:-}" ] && ip netns exec $IR tc qdisc replace dev $VI root ${IR_QDISC}
emu_up 0 $MA $MB "$PATH_A_NETEM"
if [ "$PATHS" -ge 2 ]; then
  # Paths 1..PATHS-1 (b, c, d): Iran IP 192.168.5k.1, kharej 192.168.5k.2, each
  # with its own netem (PATH_<B|C|D>_NETEM) and loss file (LOSS_EVENTS). With
  # LB_V6=1 path b is IPv6 instead (fd00:51::1 / fd00:51::2) and the kharej
  # side dials it from its own IPv6 address (same-family pairing).
  for k in $(seq 1 $((PATHS - 1))); do
    sx=$(printf "\x$(printf %x $((97 + k)))")   # b, c, d
    eval "NX=\${PATH_$(echo $sx | tr a-z A-Z)_NETEM:-}"
    ip link add ${VI}$sx netns $IR type veth peer name ${MA}$sx netns $MID
    ip link add ${VK}$sx netns $KH type veth peer name ${MB}$sx netns $MID
    if [ "${LB_V6:-0}" = 1 ] && [ $k = 1 ]; then
      ip -n $IR -6 addr add fd00:51::1/64 dev ${VI}$sx nodad
      ip -n $KH -6 addr add fd00:51::2/64 dev ${VK}$sx nodad
    else
      ip -n $IR addr add 192.168.$((50+k)).1/24 dev ${VI}$sx
      ip -n $KH addr add 192.168.$((50+k)).2/24 dev ${VK}$sx
    fi
    for x in "$IR ${VI}$sx end" "$KH ${VK}$sx end" "$MID ${MA}$sx mid" "$MID ${MB}$sx mid"; do
      set -- $x
      offl $1 $2 $3
      ip -n $1 link set dev $2 up
    done
    if [ "$LB_DIRECT" = 1 ]; then
      ip -n $KH rule add from 192.168.$((50+k)).2 table $((150 + k))
      ip -n $KH route add 192.168.50.1/32 dev ${VK}$sx src 192.168.$((50+k)).2 table $((150 + k))
    elif ! { [ "${LB_V6:-0}" = 1 ] && [ $k = 1 ]; }; then
      # replies from this Iran IP go back over its own path
      ip -n $IR rule add from 192.168.$((50+k)).1 table $((150 + k))
      ip -n $IR route add 192.168.50.2/32 dev ${VI}$sx src 192.168.$((50+k)).1 table $((150 + k))
    fi
    : > "$W/loss-$k"
    emu_up $k ${MA}$sx ${MB}$sx "$NX"
  done
  if [ "$LB_DIRECT" = 1 ]; then
    LSIDE=kharej; LIPS="192.168.50.2"; for k in $(seq 1 $((PATHS - 1))); do LIPS="$LIPS 192.168.$((50+k)).2"; done
  else
    LSIDE=iran; LIPS="192.168.50.1"
    for k in $(seq 1 $((PATHS - 1))); do
      if [ "${LB_V6:-0}" = 1 ] && [ $k = 1 ]; then LIPS="$LIPS fd00:51::1"; else LIPS="$LIPS 192.168.$((50+k)).1"; fi
    done
  fi
  LIP1=192.168.50.1; LIP2=192.168.51.1; [ "$LB_DIRECT" = 1 ] && { LIP1=192.168.50.2; LIP2=192.168.51.2; }
  # LB_WEIGHTS: one weight per path (comma-separated); fewer: equal shares
  IFS=, read -ra WS <<< "$LB_WEIGHTS"
  [ ${#WS[@]} = "$PATHS" ] || { WS=(); for k in $(seq 1 $PATHS); do WS+=($((100 / PATHS + (k <= 100 % PATHS ? 1 : 0)))); done; }
  LL="["; i=0
  for ip in $LIPS; do [ $i -gt 0 ] && LL="$LL,"; LL="$LL{\"ip\":\"$ip\",\"weight\":${WS[$i]}}"; i=$((i + 1)); done; LL="$LL]"
  [ "$LB_INIT" = one ] && LL='[{"ip":"'$LIP1'","weight":100}]'
  KHL='[{"ip":"192.168.50.2","weight":100}]'
  [ "${LB_V6:-0}" = 1 ] && KHL='[{"ip":"192.168.50.2","weight":50},{"ip":"fd00:51::2","weight":50}]'
  # LB_V6_LATE=SEC (with LB_V6=1): the IPv6 pair is not in either side's lb
  # list at the start; SEC seconds into the probe both sides get it (Iran
  # first, then kharej, each with SIGHUP) — a path added at runtime.
  if [ -n "${LB_V6_LATE:-}" ] && [ "${LB_V6:-0}" = 1 ]; then
    LL_FULL=$LL; KHL_FULL=$KHL
    LL=$(python3 -c 'import json,sys; l=[e for e in json.loads(sys.argv[1]) if ":" not in e["ip"]]; n=len(l); [e.update(weight=100//n + (1 if i < 100%n else 0)) for i,e in enumerate(l)]; print(json.dumps(l))' "$LL")
    KHL='[{"ip":"192.168.50.2","weight":100}]'
  fi
  if [ "$LB_DIRECT" = 1 ]; then
    LBJ='"lb":{"enabled":true,"mode":"'$LB_MODE'","kharej":'$LL',"iran":[{"ip":"192.168.50.1","weight":100}]},'
    KHAREJ_EXTRA="$LBJ$KHAREJ_EXTRA"
    IRAN_EXTRA='"bind_local_ip":"192.168.50.1",'"$LBJ$IRAN_EXTRA"
  else
  LBJ='"lb":{"enabled":true,"mode":"'$LB_MODE'","iran":'$LL',"kharej":'$KHL'},'
  KHAREJ_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","bind_local_ip":"192.168.50.2","sni":"lab.example.com",'"$LBJ$KHAREJ_EXTRA"
  IRAN_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","backend_addr":"builtin","cert_file":"'$W'/cert.pem","key_file":"'$W'/key.pem",'"$LBJ$IRAN_EXTRA"
  fi
fi

if [ -n "${SHARED_DN:-}" ]; then
  ip -n $KH link add ifbk$ID type ifb; ip -n $KH link set ifbk$ID up
  ip netns exec $KH tc qdisc add dev ifbk$ID root handle 1: htb default 1
  ip netns exec $KH tc class add dev ifbk$ID parent 1: classid 1:1 htb rate $SHARED_DN ceil $SHARED_DN burst 200k cburst 200k
  ip netns exec $KH tc qdisc add dev ifbk$ID parent 1:1 pfifo limit ${SHARED_Q:-2500}
  for dv in $(ip -n $KH -o link | awk -F': ' '{print $2}' | cut -d@ -f1 | grep "^$VK"); do
    ip netns exec $KH tc qdisc replace dev $dv clsact
    ip netns exec $KH tc filter add dev $dv egress matchall action mirred egress redirect dev ifbk$ID
  done
fi
if [ "${REVERSE:-0}" = 1 ] && [ "$PATHS" -lt 2 ]; then
  KHAREJ_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","sni":"lab.example.com",'"$KHAREJ_EXTRA"
  IRAN_EXTRA='"reverse":true,"addr":"192.168.50.1:2096","backend_addr":"builtin","cert_file":"'$W'/cert.pem","key_file":"'$W'/key.pem",'"$IRAN_EXTRA"
fi
if [ "$TUNNEL" = 0 ]; then
  # the bare path: idle RTT, then the heavy iperf3 straight across it
  ip netns exec $KH iperf3 -s -B 192.168.50.2 -p 5202 >/dev/null 2>&1 & echo $! >> "$W/pids"
  RTT=$(ip netns exec $IR ping -n -c 20 -i 0.2 192.168.50.2 | awk -F'= ' '/rtt/{print $2}' | cut -d' ' -f1)
  EPID=$(cat "$W/netem-0.pid" 2>/dev/null || echo 0)
  HX=""
  if [ -n "$HEAVY" ]; then
    heavy_go 192.168.50.2 5202
    python3 "$D/cpusamp.py" "$HEAVY_OMIT" "$((HEAVY_SECS - 1))" "emu=$EPID" host > "$W/emucpu.json" 2>/dev/null
    heavy_wait; HX=$(heavy_json)
  fi
  ES=$(emu_stats)
  [ "${KEEP:-0}" = 1 ] || cleanup
  printf '{"emu":"%s","rate":"%s","delay":"%s","queue":"%s","loss":"%s","rtt_idle":"%s",%s%s"emu_cpu":%s}\n' \
    "$EMU" "$RATE" "$DELAY" "$QUEUE" "$LOSS" "$RTT" "$HX" "$ES" "$(cat "$W/emucpu.json" 2>/dev/null || echo null)"
  exit 0
fi
[ "${NOWARM:-0}" = 1 ] && rm -f "/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").warm"
HPM=""; HFP=""
if [ -n "$HEAVY" ]; then
  # the heavy user's own port: Iran 8444 -> kharej 127.0.0.1:5202 (iperf3)
  PMX=""; HFP=",8444"; bi=0
  for spec in ${BG:-}; do PMX="$PMX,$((8451+bi))=127.0.0.1:$((5211+bi))"; HFP="$HFP,$((8451+bi))"
    ip netns exec $KH iperf3 -s -B 127.0.0.1 -p $((5211+bi)) >/dev/null 2>&1 & echo $! >> "$W/pids"; bi=$((bi+1)); done
  HPM='"port_map":"8444=127.0.0.1:5202'$PMX'",'
  ip netns exec $KH iperf3 -s -B 127.0.0.1 -p 5202 >/dev/null 2>&1 & echo $! >> "$W/pids"
fi
PPE=""; [ "$PPROF" = 1 ] && PPE="HS2_PPROF=127.0.0.1:6060"
KEY=$(printf 'ab%.0s' {1..32})
cat > "$W/kh.json" <<J
{"mode":"listen","carrier":"$MODE","addr":"192.168.50.2:2096","iface":"hs0",
 "local_cidr":"10.77.0.2/30","peer_ip":"10.77.0.1","mtu":1380,$KHAREJ_EXTRA
 "backend_addr":"builtin","shared_key":"$KEY","cert_file":"$W/cert.pem",
 "key_file":"$W/key.pem",$HPM"expose":"127.0.0.1:5201"}
J
cat > "$W/ir.json" <<J
{"mode":"dial","carrier":"$MODE","addr":"192.168.50.2:2096","sni":"lab.example.com",
 "iface":"hs0","local_cidr":"10.77.0.1/30","peer_ip":"10.77.0.2","mtu":1380,$IRAN_EXTRA
 "shared_key":"$KEY","forward_ports":"8443$HFP","min_links":${MIN_LINKS:-4},"max_links":${MAX_LINKS:-16}}
J
# VIA=tun: the server must also answer on the inner address, which only exists
# once hs2 has made hs0, so it listens on all addresses
PLISTEN=127.0.0.1:5201; [ "$VIA" = tun ] || [ -n "${UCALL:-}" ] && PLISTEN=0.0.0.0:5201
ip netns exec $KH "$W/probe" -server -listen $PLISTEN & echo $! >> "$W/pids"
ip netns exec $KH env HS2_NO_TUNE=1 $PPE $KH_ENV "$W/hs2kh" run -c "$W/kh.json" >"$W/kh.log" 2>&1 & KHPID=$!; echo $KHPID >> "$W/pids"
sleep 1
ip netns exec $IR env HS2_NO_TUNE=1 $PPE $IR_ENV "$W/hs2" run -c "$W/ir.json" >"$W/ir.log" 2>&1 & IRPID=$!; echo $IRPID >> "$W/pids"
# wait until the forwarded port answers (max ~15s)
for i in $(seq 1 60); do
  ip netns exec $IR bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null && break
  sleep 0.25
done
PADDR=127.0.0.1:8443; TUNX=""
if [ "$VIA" = tun ]; then
  PADDR=10.77.0.2:5201
  for i in $(seq 1 60); do
    ip netns exec $IR bash -c 'exec 3<>/dev/tcp/10.77.0.2/5201' 2>/dev/null && break
    sleep 0.25
  done
  ROUTE=$(ip -n $IR route get 10.77.0.2 2>&1 | head -1 | tr -s ' ' | tr -d '"' | sed 's/ *$//')
  case "$ROUTE" in
    *"dev hs0 "*|*"dev hs0") ;;
    *) echo "VIA=tun: 10.77.0.2 is not routed through hs0: $ROUTE" >&2; cleanup; exit 1 ;;
  esac
  tunb(){ ip netns exec $IR cat /sys/class/net/hs0/statistics/$1 2>/dev/null || echo 0; }
fi
sleep 2
PINGF="$W/ping.txt"; : > "$PINGF"
if ip -n $IR link show hs0 >/dev/null 2>&1; then
  # with LATE the ping starts with the late streams and ends with the probe
  PW=$(python3 -c "import sys; t=float(sys.argv[1].rstrip('s')); l=float(sys.argv[2].rstrip('s') or 0); print(max(int(t-l),1))" "$T" "$LATE")
  (sleep "${LATE%s}"; exec ip netns exec $IR ping -n -D -i ${PING_I:-0.2} -w "$PW" 10.77.0.2) > "$PINGF" 2>&1 &
  PP=$!
fi
SF=/run/hs2/$(echo "$W/ir.json" | sed "s|^/||; s|/|-|g").status.json
# the pool's link count once a second while the probe runs (links_max)
(while :; do python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('links',0))" "$SF" 2>/dev/null; sleep 1; done) > "$W/links.txt" &
LP=$!; echo $LP >> "$W/pids"
SFK=/run/hs2/$(echo "$W/kh.json" | sed "s|^/||; s|/|-|g").status.json
SFD=$SFK; [ "$LB_DIRECT" = 1 ] && SFD=$SF   # the dialing side's status
if [ "$PATHS" -ge 2 ]; then
  # per second: "t path=links/target(src) ..." on the dialing side
  (t0=$(date +%s); while :; do python3 -c "import json,sys; print(sys.argv[2], ' '.join('%s=%d/%d%s' % (p['path'], p['links'], p['target'], '' if p.get('state','ok')=='ok' else ':'+p['state']) for p in json.load(open(sys.argv[1])).get('lb',[])))" "$SFD" $(( $(date +%s) - t0 )) 2>/dev/null; sleep 1; done) > "$W/lbtime.txt" &
  echo $! >> "$W/pids"
  (while :; do python3 -c "import json,sys; print(' '.join(str(p['links']) for p in json.load(open(sys.argv[1])).get('lb',[])))" "$SFD" 2>/dev/null; sleep 1; done) > "$W/lbpaths.txt" &
  echo $! >> "$W/pids"
fi
if [ -n "$LB_EVENTS" ] && [ "$PATHS" -ge 2 ]; then
  if [ "$LB_DIRECT" = 1 ]; then LCFG=$W/kh.json; LPID=$KHPID; else LCFG=$W/ir.json; LPID=$IRPID; fi
  (t0=$(date +%s)
   for ev in $LB_EVENTS; do
     at=${ev%%:*}; spec=${ev#*:}
     while [ $(( $(date +%s) - t0 )) -lt "$at" ]; do sleep 0.2; done
     python3 - "$LCFG" "$LSIDE" "$LIP1" "$LIP2" "$spec" <<'PY'
import json, sys
f, side, a, b, spec = sys.argv[1:]
c = json.load(open(f))
if spec == 'one':
    c['lb'][side] = [{'ip': a, 'weight': 100}]
else:
    wa, wb = spec[2:].split(',')
    c['lb'][side] = [{'ip': a, 'weight': int(wa)}, {'ip': b, 'weight': int(wb)}]
json.dump(c, open(f, 'w'))
PY
     kill -HUP "$LPID"; echo "$at $spec" >> "$W/lbevents.txt"
   done) &
  echo $! >> "$W/pids"
fi
# LOSS_EVENTS="SEC:K:LOSS ..." (PATHS>=2): at SEC seconds into the probe, path
# K (0 = a, 1 = b, ...) changes its random loss to LOSS each way ("-" = back to
# LOSS / its netem flags). LB_SAMPLE=1: once a second, both sides' per-path
# status as one JSON line in $W/lbsamp.jsonl (LOGDIR: RUNTAG-lbsamp.jsonl).
if [ -n "${LB_V6_LATE:-}" ] && [ "${LB_V6:-0}" = 1 ] && [ -n "${LL_FULL:-}" ]; then
  (t0=$(date +%s)
   while [ $(( $(date +%s) - t0 )) -lt "$LB_V6_LATE" ]; do sleep 0.2; done
   python3 - "$W/ir.json" iran "$LL_FULL" <<'PY'
import json, sys
f, side, l = sys.argv[1:]
c = json.load(open(f)); c['lb'][side] = json.loads(l); json.dump(c, open(f, 'w'))
PY
   kill -HUP "$IRPID"; sleep 2
   python3 - "$W/kh.json" kharej "$KHL_FULL" <<'PY'
import json, sys
f, side, l = sys.argv[1:]
c = json.load(open(f)); c['lb'][side] = json.loads(l); json.dump(c, open(f, 'w'))
PY
   kill -HUP "$KHPID"; echo "$LB_V6_LATE v6-added" >> "$W/lbevents.txt") &
  echo $! >> "$W/pids"
fi
if [ -n "${LOSS_EVENTS:-}" ] && [ "$PATHS" -ge 2 ]; then
  (t0=$(date +%s)
   for ev in $LOSS_EVENTS; do
     at=${ev%%:*}; rest=${ev#*:}; k=${rest%%:*}; l=${rest#*:}
     while [ $(( $(date +%s) - t0 )) -lt "$at" ]; do sleep 0.2; done
     emu_loss $k "$l"; echo "$at $k $l" >> "$W/lossevents.txt"
   done) &
  echo $! >> "$W/pids"
fi
# lb74: NETEM_EVENTS="SEC:K:netem_args_with_underscores ..." — at SEC seconds, path K's
# netem extra args become these (both directions), e.g. a standing queue.
if [ -n "${NETEM_EVENTS:-}" ] && [ "$EMU" = kernel ]; then
  (t0=$(date +%s)
   for ev in $NETEM_EVENTS; do
     at=${ev%%:*}; rest=${ev#*:}; k=${rest%%:*}; x=${rest#*:}; x=${x//_/ }
     while [ $(( $(date +%s) - t0 )) -lt "$at" ]; do sleep 0.2; done
     read -r a b x0 < "$W/emu-$k"
     for dv in $a $b; do ip netns exec $MID tc qdisc change dev $dv root netem $(knetem "$LOSS") $x; done
     echo "$at $k $x" >> "$W/netemevents.txt"
   done) &
  echo $! >> "$W/pids"
fi
rm -f "$W/lbsamp.jsonl" "$W/lossevents.txt"
if [ "${LB_SAMPLE:-0}" = 1 ] && [ "$PATHS" -ge 2 ]; then
  (t0=$(date +%s.%N); while :; do python3 - "$SF" "$SFK" "$t0" <<'PY' 2>/dev/null; sleep 1; done) >> "$W/lbsamp.jsonl" &
import json, sys, time
def rd(f):
    try: return json.load(open(f))
    except Exception: return {}
ir, kh = rd(sys.argv[1]), rd(sys.argv[2])
keep = ("local", "remote", "links", "users", "down_mbit", "new_pct", "auto_factor", "auto_reason", "eff_pct", "loss_down_pct", "target")
row = lambda p: {k: p[k] for k in keep if k in p}
print(json.dumps({"t": round(time.time() - float(sys.argv[3]), 1),
                  "ir": [row(p) for p in ir.get("lb_paths") or []], "irp": [row(p) for p in ir.get("lb_pairs") or []],
                  "kh": [row(p) for p in kh.get("lb_paths") or []], "verd": (ir.get("lb_verdicts") or []) + (kh.get("lb_verdicts") or []),
                  "links_ir": ir.get("links", 0), "users": ir.get("users", 0)}))
PY
  echo $! >> "$W/pids"
fi
UFLAG=""; [ "$UDP" = 1 ] && UFLAG="-udp"
PX=""
[ -n "$CALL" ] && PX="$PX -call $CALL"
if [ -n "${UCALL:-}" ]; then
  PX="$PX -ucall $UCALL -ucalladdr 10.77.0.2:5201 ${UCALL_EXTRA:-}"
  for i in $(seq 1 60); do
    ip netns exec $IR bash -c 'exec 3<>/dev/tcp/10.77.0.2/5201' 2>/dev/null && break
    sleep 0.25
  done
fi
[ "$GAME" = 1 ] && PX="$PX -game"
[ "$WEB" = 1 ] && PX="$PX -web"
[ "$LATE" != 0 ] && PX="$PX -late $LATE"
[ "$CROWD" != 0 ] && PX="$PX -crowd $CROWD -crowdevery $CROWD_EVERY"
[ "$JOURNEY" != 0 ] && PX="$PX -journey $JOURNEY $JOURNEY_EXTRA"
[ "$VIA" = tun ] && { RX0=$(tunb rx_bytes); TX0=$(tunb tx_bytes); }
KILLP=""; rm -f "$W/kill.json"
if [ "${KILL_AT:-0}" != 0 ]; then
  python3 "$D/killrebuild.py" $IR $KH 2096 "${KILL_AT%s}" "${KILL_PCT:-75}" "$W/kill.json" & KILLP=$!
fi
ip netns exec $IR sysctl -q -w net.ipv4.ip_local_reserved_ports=41000-41063; bi=0; date +%s.%N > "$W/bgstart"
for spec in ${BG:-}; do bd=${spec%%:*}; br=${spec#*:}; BR=""; [ "$bd" = d ] && BR="-R"
  ip netns exec $IR iperf3 -c 127.0.0.1 -p $((8451+bi)) -t ${BG_SECS:-20} -b ${br}M $BR -J > "$W/bg$bi.json" 2> "$W/bg$bi.err" & echo $! >> "$W/pids"
  bi=$((bi+1)); sleep 0.3; done
# the heavy user starts with the probe (8443 answering means 8444 listens too)
if [ -n "$HEAVY" ] && [ "${HEAVY_CTL:-}" = direct ]; then
  # s45: the iperf3 control connection goes over the bare path (not a link, so the
  # link kill cannot close it); the data streams (--cport 41000+i) are DNATed to 8444.
  ip netns exec $KH python3 "$D/tcprelay.py" 192.168.50.2 5299 127.0.0.1 5202 >/dev/null 2>&1 & echo $! >> "$W/pids"
  ip netns exec $IR python3 "$D/tcprelay.py" 127.0.0.1 8450 192.168.50.2 5299 >/dev/null 2>&1 & echo $! >> "$W/pids"
  ip netns exec $IR iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 8450 -m multiport --sports 41000:41063 -j DNAT --to-destination 127.0.0.1:8444
  sleep 0.3
  HEAVY_XARGS="--cport 41000" heavy_go 127.0.0.1 8450
elif [ -n "$HEAVY" ]; then heavy_go 127.0.0.1 8444; fi
OUT=$(ip netns exec $IR "$W/probe" -addr $PADDR -bulk "$BULK" -up "$UP" -t "$T" $UFLAG $PX ${PROBE_EXTRA:-})
if [ "$VIA" = tun ]; then
  # Iran side hs0: rx = what came back through the TUN (downloads, replies), tx = what went into it
  TUNX=$(printf '"via":"tun","tun_route":"%s","tun_rx_mb":%s,"tun_tx_mb":%s,' "$ROUTE" \
    "$(python3 -c "import sys; print(round((int(sys.argv[2])-int(sys.argv[1]))/1e6,1))" "$RX0" "$(tunb rx_bytes)")" \
    "$(python3 -c "import sys; print(round((int(sys.argv[2])-int(sys.argv[1]))/1e6,1))" "$TX0" "$(tunb tx_bytes)")")
fi
[ -n "${PP:-}" ] && wait "$PP" 2>/dev/null
kill $LP 2>/dev/null
LMAX=$(sort -n "$W/links.txt" 2>/dev/null | tail -1)
PING=$(awk -F'= ' '/rtt/{split($2,a,"/"); print a[2]"/"a[3]}' "$PINGF")
PLOSS=$(grep -o '[0-9.]*% packet loss' "$PINGF" | cut -d% -f1)
# per-reply hs0 ping times -> "ping_n":..,"ping_p50":..,... (empty without a TUN)
PSTAT=$(python3 - "$PINGF" <<'PY'
import re, sys
v = sorted(float(x) for x in re.findall(r'time=([0-9.]+) ms', open(sys.argv[1]).read()))
if v:
    p = lambda q: v[int(q * (len(v) - 1))]
    over = 100 * sum(x > 120 for x in v) / len(v)
    print('"ping_n":%d,"ping_p50":%.1f,"ping_p90":%.1f,"ping_p99":%.1f,"ping_max":%.1f,"ping_over120_pct":%.2f,'
          % (len(v), p(.5), p(.9), p(.99), v[-1], over))
PY
)
REAPS=$(grep -cE 'reap:|rebuilt|link down' "$W/ir.log")
LINKS=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get(\"links\",0))" "$SF" 2>/dev/null || echo 0)
[ "${LINKS:-0}" -gt "${LMAX:-0}" ] 2>/dev/null && LMAX=$LINKS
QCAPS=$(grep -c "tunnel egress capped" "$W/kh.log" 2>/dev/null)
QREL=$(grep -c "not the tunnel's own" "$W/kh.log" 2>/dev/null)
LBX=""
if [ "$PATHS" -ge 2 ]; then
  # per-path link counts on the dialing (kharej) side: at the end, and the max
  LBX=$(python3 - "$W/lbpaths.txt" "$SFD" <<'PY'
import json, sys
rows = [list(map(int, l.split())) for l in open(sys.argv[1]) if l.split()]
try:
    st = json.load(open(sys.argv[2]))
    end = [p['links'] for p in st.get('lb', [])]; text = st.get('lb_text', '')
except Exception:
    end, text = (rows[-1] if rows else []), ''
mx = [max(r[i] for r in rows if len(r) > i) for i in range(len(end))] if rows else []
half = rows[len(rows)//2:]
avg = [round(sum(r[i] for r in half if len(r) > i) / max(1, len(half)), 1) for i in range(len(end))]
print('"lb_paths_end":%s,"lb_paths_max":%s,"lb_paths_avg2h":%s,"lb_text":"%s",' % (json.dumps(end), json.dumps(mx), json.dumps(avg), text))
PY
)
fi
KILLX=""
if [ -n "$KILLP" ]; then
  wait "$KILLP" 2>/dev/null
  [ -f "$W/kill.json" ] && KILLX='"kill":'$(cat "$W/kill.json")','
fi
if [ -n "${LOGDIR:-}" ]; then  # keep both sides' logs: LOGDIR/RUNTAG-ir.log, -kh.log
  mkdir -p "$LOGDIR"; cp "$W/ir.log" "$LOGDIR/${RUNTAG:-run}-ir.log"; cp "$W/kh.log" "$LOGDIR/${RUNTAG:-run}-kh.log"
  [ -f "$W/lbsamp.jsonl" ] && cp "$W/lbsamp.jsonl" "$LOGDIR/${RUNTAG:-run}-lbsamp.jsonl"
  [ -f "$W/lossevents.txt" ] && cp "$W/lossevents.txt" "$LOGDIR/${RUNTAG:-run}-lossevents.txt"
fi
heavy_wait; HX=$(heavy_json); ES=$(emu_stats)
# smuxwin: peak RSS of both hs2 and the adaptive-window budget from their status
RSSX=$(python3 - "${IRPID:-0}" "${KHPID:-0}" "$SF" "$SFK" <<"PY"
import json, sys
def hwm(pid):
    try:
        for l in open("/proc/%s/status" % pid):
            if l.startswith("VmHWM:"): return round(int(l.split()[1]) / 1024, 1)
    except Exception: pass
    return 0
def st(f):
    try: d = json.load(open(f))
    except Exception: return {}
    return {k: d.get(k, 0) for k in ("smux_win_peak_mib", "obs_smux_win_grows", "obs_smux_win_denied", "obs_smux_win_shrinks")}
print("\"rss_ir_mb\":%s,\"rss_kh_mb\":%s,\"aw_ir\":%s,\"aw_kh\":%s," % (hwm(sys.argv[1]), hwm(sys.argv[2]), json.dumps(st(sys.argv[3])), json.dumps(st(sys.argv[4]))))
PY
)
HX="$RSSX$HX"
[ "${KEEP:-0}" = 1 ] || cleanup
printf '{"qcaps":%s,"qrel":%s,"links_end":%s,"links_max":%s,"mode":"%s","emu":"%s","rate":"%s","delay":"%s","loss":"%s","flowrate":"%s","bulk":%s,"ping_avg_max":"%s","ping_loss":"%s",%s%s"link_churn":%s,%s%s%s%s"probe":%s}\n' \
  "${QCAPS:-0}" "${QREL:-0}" "${LINKS:-0}" "${LMAX:-0}" "$MODE" "$EMU" "$RATE" "$DELAY" "$LOSS" "$FLOWRATE" "$BULK" "${PING:-}" "${PLOSS:-}" "${PSTAT:-}" "$TUNX" "$REAPS" "$LBX" "$KILLX" "$HX" "$ES" "${OUT:-null}"

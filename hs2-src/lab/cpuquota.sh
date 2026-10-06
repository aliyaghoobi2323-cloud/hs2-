#!/usr/bin/env bash
# A sending server short of CPU, reproducibly: the field regime of the Phase W
# report (Kharej server saturated, carriers never pushing, all in startup).
#
# Two network namespaces joined by a veth, real hs2 dgtun on each side
# (reverse, tun over icmp by default). The Kharej hs2 — the sender of the
# downloads — runs in a cgroup with a hard CPU quota (QUOTA cores, enforced
# every PERIOD_US) and on its own CPUs; the Iran hs2, the load generators and
# ping run on other CPUs, and the Iran veth's receive work is steered there
# too (RPS), so the quota is all the sender gets. Unlike busy loops beside it
# (CFS shares, which move with nice and with the load), a quota gives the same
# CPU every run, so two builds compare on Mbit/s per CPU-second.
#
#   QUOTA=0.6 lab/cpuquota.sh            # 0.6 of a core for the Kharej hs2
#   SHARE=0.3 lab/cpuquota.sh            # 30% of its CPUs, the rest busy
#   QUOTA=0 RATE=30mbit lab/cpuquota.sh  # no quota, a 30 Mbit/s path instead
#
# SHARE is how a systemd server splits its CPU between services (each in its
# own cgroup, weighted alike): the Kharej hs2 and busy loops beside it (one
# per CPU) in two cgroups whose weights give hs2 that share of its CPUs while
# the loops want them. Unlike QUOTA — which stops all of hs2's threads at once
# when it is spent — the scheduler then takes CPUs from hs2's threads one at a
# time, as on a server shared with another tunnel: a thread can be put aside
# while holding a lock its siblings wait for. Unlike busy loops in the same
# cgroup (the scheduler's share per thread, which moved with hs2's thread
# count), the group weights give hs2 the same share every run.
#
# Env [defaults]: BIN [built from this tree] IBIN [BIN: the Iran side's]
#   ENCAP [icmp] QUOTA [0.6 cores; 0 = none] PERIOD_US [10000]
#   SHARE [unset: a fraction of the Kharej CPUs, instead of QUOTA]
#   CPUS_K [0,1] CPUS_I [2,3] LINKS [4: min_links = max_links]
#   CONNS [8 downloads] DUR [30 s] RATE [none: a tbf on the Kharej egress]
#   CONNRATE [100000000: bytes/s each download offers; lower it to compare
#   builds at the same throughput]
#   TRACE [0: 1 = a 5 s Go execution trace of the Kharej hs2 under load]
#   KENV / IENV ["": extra VAR=value for the Kharej / Iran hs2, space-separated]
#   W [/tmp/hs2cpuq] LAB_ID [""]
# Prints a summary and one JSON line (the last). Needs root, iproute2, python3,
# cgroup v1 (cpu) or v2.
set -u
D=$(cd "$(dirname "$0")" && pwd)
ID=${LAB_ID:-}
W=${W:-/tmp/hs2cpuq}$ID
A=cqi$ID B=cqk$ID VA=cqA$ID VB=cqB$ID
ENCAP=${ENCAP:-icmp} QUOTA=${QUOTA:-0.6} PERIOD_US=${PERIOD_US:-10000} SHARE=${SHARE:-}
[ -n "$SHARE" ] && QUOTA=0
CPUS_K=${CPUS_K:-0,1} CPUS_I=${CPUS_I:-2,3} LINKS=${LINKS:-4}
CONNS=${CONNS:-8} DUR=${DUR:-30} CONNRATE=${CONNRATE:-100000000} RATE=${RATE:-} TRACE=${TRACE:-0}
CG="" CGH=""

cleanup(){
  for p in $(cat "$W"/pids 2>/dev/null); do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in $(cat "$W"/pids 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  for n in $A $B; do ip netns pids $n 2>/dev/null | xargs -r kill -9 2>/dev/null; done
  ip netns del $A 2>/dev/null; ip netns del $B 2>/dev/null
  for p in $(cat "$CGH/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
  sleep 0.1
  [ -n "$CG" ] && rmdir "$CG" 2>/dev/null
  [ -n "$CGH" ] && rmdir "$CGH" 2>/dev/null
  return 0
}
mkdir -p "$W"; cleanup; rm -f "$W"/pids "$W"/*.log "$W"/*.txt
trap cleanup EXIT
if [ -z "${BIN:-}" ]; then
  (cd "$D/.." && CGO_ENABLED=0 go build -o "$W/hs2" ./cmd/hs2) || exit 1
  BIN="$W/hs2"
fi
IBIN=${IBIN:-$BIN}

# The quota's cgroup: v1 cpu controller, else a v2 group with cpu enabled.
if [ -n "$SHARE" ]; then
  # weights: hs2 : loops = SHARE : 1-SHARE (v1 shares, or v2 weights)
  WH=$(python3 -c "print(max(2,int(1024*float('$SHARE')/(1-float('$SHARE')))))")
  if [ -f /sys/fs/cgroup/cpu/cpu.shares ]; then
    CG=/sys/fs/cgroup/cpu/hs2cpuq$ID; CGH=/sys/fs/cgroup/cpu/hs2hog$ID
    mkdir -p "$CG" "$CGH" || exit 1
    echo "$WH" > "$CG/cpu.shares"; echo 1024 > "$CGH/cpu.shares"
  elif [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    CG=/sys/fs/cgroup/hs2cpuq$ID; CGH=/sys/fs/cgroup/hs2hog$ID
    echo +cpu > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null
    mkdir -p "$CG" "$CGH" || exit 1
    echo $((WH * 100 / 1024 + 1)) > "$CG/cpu.weight"; echo 100 > "$CGH/cpu.weight"
  else
    echo "no cpu cgroup for the shares" >&2; exit 1
  fi
  for c in $(echo "$CPUS_K" | tr ',' ' '); do
    sh -c 'echo $$ > "$1/cgroup.procs"; exec taskset -c "$2" sh -c "while :; do :; done"' _ "$CGH" "$c" &
    echo $! >> "$W/pids"
  done
elif [ "$QUOTA" != 0 ]; then
  Q=$(python3 -c "print(int(float('$QUOTA')*$PERIOD_US))")
  if [ -d /sys/fs/cgroup/cpu ] && [ -f /sys/fs/cgroup/cpu/cpu.cfs_quota_us ]; then
    CG=/sys/fs/cgroup/cpu/hs2cpuq$ID; mkdir -p "$CG" || exit 1
    echo "$PERIOD_US" > "$CG/cpu.cfs_period_us"; echo "$Q" > "$CG/cpu.cfs_quota_us"
  elif [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    CG=/sys/fs/cgroup/hs2cpuq$ID; mkdir -p "$CG" || exit 1
    echo +cpu > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null
    echo "$Q $PERIOD_US" > "$CG/cpu.max" || exit 1
  else
    echo "no cpu cgroup to hold the quota" >&2; exit 1
  fi
fi

ip netns add $A; ip netns add $B
ip link add $VA netns $A type veth peer name $VB netns $B
ip -n $A addr add 10.42.0.1/24 dev $VA; ip -n $B addr add 10.42.0.2/24 dev $VB
for n in $A $B; do ip -n $n link set lo up; done
ip -n $A link set $VA up; ip -n $B link set $VB up
# The Iran side's receive work on the Iran CPUs, not on the sender's.
IMASK=$(python3 -c "print(format(sum(1<<int(c) for c in '$CPUS_I'.split(',')),'x'))")
ip netns exec $A sh -c "echo $IMASK > /sys/class/net/$VA/queues/rx-0/rps_cpus" 2>/dev/null
if [ -n "$RATE" ]; then
  ip netns exec $B tc qdisc add dev $VB root tbf rate "$RATE" burst 32kb latency 300ms
fi

KEY=$(printf 'cd%.0s' {1..32})
cat > "$W/iran.json" <<J
{"mode":"dial","carrier":"dgtun","encap":"$ENCAP","reverse":true,"addr":"0.0.0.0:30420","iface":"cqi0","local_cidr":"10.142.0.1/30","peer_ip":"10.142.0.2","mtu":1280,"shared_key":"$KEY","forward_ports":"","user_listen_ip":"","min_links":$LINKS,"max_links":$LINKS}
J
cat > "$W/kharej.json" <<J
{"mode":"listen","carrier":"dgtun","encap":"$ENCAP","reverse":true,"addr":"10.42.0.1:30420","iface":"cqk0","local_cidr":"10.142.0.2/30","peer_ip":"10.142.0.1","mtu":1280,"shared_key":"$KEY","expose":"127.0.0.1:9","min_links":$LINKS,"max_links":$LINKS}
J
ip netns exec $A env HS2_NO_TUNE=1 ${IENV:-} taskset -c "$CPUS_I" "$IBIN" run -c "$W/iran.json" >"$W/iran.log" 2>&1 & echo $! >> "$W/pids"
sleep 1
ip netns exec $B env HS2_NO_TUNE=1 HS2_PPROF=127.0.0.1:30421 ${KENV:-} taskset -c "$CPUS_K" "$BIN" run -c "$W/kharej.json" >"$W/kharej.log" 2>&1 &
echo $! >> "$W/pids"
KPID=""
for _ in $(seq 1 40); do
  KPID=$(pgrep -f "run -c $W/kharej.json" | while read -r p; do [ "$(readlink -f /proc/$p/exe 2>/dev/null)" = "$(readlink -f "$BIN")" ] && echo "$p"; done | head -1)
  [ -n "$KPID" ] && break; sleep 0.05
done
[ -z "$KPID" ] && { echo "the Kharej hs2 did not start" >&2; cat "$W/kharej.log" >&2; exit 1; }
# Into the quota's cgroup from out here (ip netns exec remounts /sys, which
# hides the cgroup tree): cgroup.procs moves every thread, and the threads it
# makes later inherit it. Only its first moments run outside the quota.
if [ -n "$CG" ]; then
  echo "$KPID" > "$CG/cgroup.procs" || { echo "could not move the Kharej hs2 into $CG" >&2; exit 1; }
fi
for _ in $(seq 1 60); do
  ip netns exec $A ping -c 1 -W 1 10.142.0.2 >/dev/null 2>&1 && break; sleep 0.5
done
sleep 3

ticks(){ awk '{print $14+$15}' /proc/"$1"/stat; }
sticks(){ awk '{print $15}' /proc/"$1"/stat; }
odisc(){ ip netns exec $B awk '/^Ip:/{if(h){print $(i)}else{for(i=1;i<=NF;i++)if($i=="OutDiscards")break;h=1}}' /proc/net/snmp; }
thr(){ if [ -z "$CG" ]; then echo 0; elif [ -f "$CG/cpu.stat" ]; then awk '/^nr_throttled/{print $2}' "$CG/cpu.stat"; fi; }
ip netns exec $A taskset -c "$CPUS_I" ping -c 40 -i 0.05 -W 2 10.142.0.2 > "$W/ping_idle.txt" 2>&1

ip netns exec $B taskset -c "$CPUS_I" python3 "$D/tcpload.py" server 10.142.0.2 30425 "$CONNRATE" & echo $! >> "$W/pids"
sleep 0.5
k0=$(ticks "$KPID"); s0=$(sticks "$KPID"); o0=$(odisc); h0=$(thr); t0=$(date +%s.%N)
ip netns exec $A taskset -c "$CPUS_I" python3 "$D/tcpload.py" client 10.142.0.2 30425 "$CONNS" "$DUR" "$W/load.json" & LP=$!
( sleep 5; ip netns exec $A taskset -c "$CPUS_I" ping -c $(( (DUR-8)*10 )) -i 0.1 -W 2 10.142.0.2 > "$W/ping.txt" 2>&1 ) & PGP=$!
( for s in $(seq 6 6 "$DUR"); do
    sleep 6; ip netns exec $B timeout 5 "$BIN" status -c "$W/kharej.json" > "$W/st$s.txt" 2>&1
  done ) & STP=$!
if [ "$TRACE" = 1 ]; then
  ( sleep 12; ip netns exec $B timeout 15 curl -s -o "$W/trace.out" "http://127.0.0.1:30421/debug/pprof/trace?seconds=5" ) & TRP=$!
fi
wait $LP
k1=$(ticks "$KPID"); s1=$(sticks "$KPID"); o1=$(odisc); h1=$(thr); t1=$(date +%s.%N)
wait $PGP $STP; [ "$TRACE" = 1 ] && wait $TRP

python3 - "$W" "$((k1-k0))" "$(python3 -c "print($t1-$t0)")" "$((o1-o0))" "$((h1-h0))" "$QUOTA" "$ENCAP" "$LINKS" "$CONNS" "${RATE:-none}" "${SHARE:-0}" "$((s1-s0))" <<'P'
import json, re, sys, glob, os
w, ticks, el, odisc, thr, quota, encap, links, conns, rate, share, sticks = sys.argv[1:]
el = float(el); cpu_s = int(ticks) / 100.0
d = json.load(open(os.path.join(w, "load.json")))
tot = [x for x in d["tot"]]
mbit = sum(x for x in tot if x > 0) * 8 / d["secs"] / 1e6
late = sum(d["late"]) * 8 / max(d["secs"] - 10, 1) / 1e6
def pings(f):
    try: t = open(f).read()
    except OSError: return None
    v = sorted(float(m) for m in re.findall(r'time=([\d.]+)', t))
    m = re.search(r'(\d+) packets transmitted, (\d+) received', t)
    tx, rx = (int(m.group(1)), int(m.group(2))) if m else (0, 0)
    q = lambda p: v[min(len(v) - 1, int(p * len(v)))] if v else -1
    return {"n": tx, "loss_pct": round(100 * (tx - rx) / max(tx, 1), 1), "p50": q(.5), "p99": q(.99)}
st = sorted(glob.glob(os.path.join(w, "st*.txt")), key=lambda f: int(re.findall(r'st(\d+)', f)[0]))
lines = {}
flags_s = flags_p = n = 0
for f in st:
    t = open(f).read()
    for k in ("sending:", "carriers:", "cpu:", "drops:", "pool:"):
        m = re.search(r'^\s*' + k + r'\s*(.*)$', t, re.M)
        if m: lines.setdefault(k, []).append(m.group(1))
    m = re.search(r'^\s*carriers:\s*(.*)$', t, re.M)
    if m:
        for e in re.findall(r'\d+:\w+:\d+/[\d.]+% r[\d.]+/bw[\d.]+ (\S+)', m.group(1)):
            n += 1; flags_s += "S" in e; flags_p += "P" in e
held = [float(x) for x in re.findall(r'pacer room (\d+)%', " ".join(lines.get("sending:", [])))]
res = {"encap": encap, "quota_cores": float(quota), "share": float(share), "links": int(links), "conns": int(conns), "rate": rate,
       "mbit": round(mbit, 1), "mbit_late": round(late, 1), "cpu_s": round(cpu_s, 1),
       "cpu_cores": round(cpu_s / el, 2), "sys_share": round(int(sticks) / max(int(ticks), 1), 2), "mbit_per_cpu_s": round(mbit * d["secs"] / max(cpu_s, 0.01), 1),
       "failed_conns": sum(1 for x in tot if x < 0), "max_gap_s": round(max(d["maxgap"]), 2),
       "throttled_periods": int(thr), "out_discards": int(odisc),
       "carrier_samples": n, "share_S": round(flags_s / max(n, 1), 2), "share_P": round(flags_p / max(n, 1), 2),
       "send_held_pct_mean": round(sum(held) / len(held), 1) if held else None,
       "ping_idle": pings(os.path.join(w, "ping_idle.txt")), "ping_load": pings(os.path.join(w, "ping.txt"))}
for k, v in lines.items():
    print("%-10s %s" % (k, v[len(v) // 2]))
print(json.dumps(res))
P
grep -HiE "panic|fatal" "$W/iran.log" "$W/kharej.log" | head -5
exit 0

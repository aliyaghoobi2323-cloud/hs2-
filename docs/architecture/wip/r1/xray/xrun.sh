#!/bin/bash
# xrun.sh PLAN OUT: step 2 of R1 round 3 (xray) on the shared rig
# (../../rig-scale/rig.sh, RIG_ID=x). One PLAN line per hs2 pair:
#
#   BIN=old N=1 LOSS=0 VARIANTS=xa,xb2,gr REP=1
#
# Each line: fresh rig namespaces (RATE 50, RTT 72, fq_codel, LOSS % each
# way, one user address 10.100.0.1), fresh hs2 key and cert, the edge in
# r-edge and the exit in r-exit (reverse l3mtcp, min_links = max_links = N,
# port_map 5311 -> 127.0.0.1:5211 and 5312 -> 127.0.0.1:5212), then in
# r-exit the REALITY target, the traffic server and one xray server (kind xr
# on 5211, the second kind of the variants on 5212), and per variant, in the
# order given, in r-edge one xray client (SOCKS 127.0.0.1:1080, source
# 10.100.0.1, to 10.201.0.1:5311 or :5312) with xtraffic.py's "lat"
# timeline: a TCP and a UDP game (50 Hz, 64 B each way) from 0 to
# WARM + DUR + 3 s and one download from WARM for DUR s. The edge's `hs2
# status` is taken during the download and after it. One JSON line per
# variant run goes to OUT/res.jsonl (xana.py), raw files to OUT/raw/ID/.
# Binaries: old $OLD_BIN, new $NEW_BIN with $NEW_ENV (the R1 candidate).
set -u
D=$(cd "$(dirname "$0")" && pwd)
RIG=${RIG:-$(cd "$D/../../rig-scale" && pwd)}
PLAN=${1:?usage: xrun.sh PLAN OUT}; OUT=${2:?usage: xrun.sh PLAN OUT}
mkdir -p "$OUT/raw"; OUT=$(cd "$OUT" && pwd)
KEYS=${KEYS:-/root/rxray/keys}; XR=${XR:-/root/rxray/bin/xray}
OLD_BIN=${OLD_BIN:-/root/r2j/bins/hs2-base}; NEW_BIN=${NEW_BIN:-/root/r2j/bins/hs2-b2n}
NEW_ENV=${NEW_ENV:-HS2_TUNE_QUIET_EXTRA=1}
WARM=${WARM:-5}; DUR=${DUR:-20}; RATE=${RATE:-50}; RTT=${RTT:-72}
export RIG_ID=${RIG_ID:-x}
E=r-edge$RIG_ID; X=r-exit$RIG_ID
WORK=/tmp/rxray-rig$RIG_ID
LOG=$OUT/run.log
PIDS=()
declare -A KINDOF=([xa]=xr [xc16]=xr [xb1]=xr [xm3]=xr [xb2]=xr [xt]=xt [vv]=vv [vm]=vm [tj]=tj [ws]=ws [hu]=hu [gr]=gr [grm]=gr [ss]=ss [kc]=kc)

log() { echo "$(date +%T) $*" | tee -a "$LOG"; }

cleanup() {
  local p s
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  sleep 0.3
  for p in "${PIDS[@]}"; do kill -9 "$p" 2>/dev/null; done
  PIDS=()
  "$RIG/rig.sh" down
  s=/run/hs2/$(echo "${WORK#/}" | tr / -)
  rm -f "$s-edge.json.status.json" "$s-exit.json.status.json" "$s-edge.json.warm" "$s-exit.json.warm"
  rm -rf "$WORK"
}
trap 'cleanup; exit 130' INT TERM
trap 'cleanup' EXIT

# hs2 configs as run.sh's mkconf (the field's reverse l3mtcp tunnel)
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

stat_edge() { "$bp" status -c "$WORK/edge.json" > "$1" 2>&1; }

one_line() {
  local BIN N LOSS VARIANTS REP tok bp benv pe px n v k port second r id t0 sp
  BIN=old; N=1; LOSS=0; VARIANTS=xa; REP=1
  for tok in $1; do
    [[ "$tok" =~ ^(BIN|N|LOSS|VARIANTS|REP)=[A-Za-z0-9_.,]+$ ]] || { log "bad token $tok"; return 1; }
    printf -v "${tok%%=*}" '%s' "${tok#*=}"
  done
  case $BIN in old) bp=$OLD_BIN; benv="" ;; new) bp=$NEW_BIN; benv=$NEW_ENV ;; *) log "bad BIN $BIN"; return 1 ;; esac
  # the second kind on 5212: the first non-xr kind among the variants (xr if none)
  second=xr
  for v in ${VARIANTS//,/ }; do k=${KINDOF[$v]}; [ "$k" != xr ] && { second=$k; break; }; done
  log "line: BIN=$BIN N=$N LOSS=$LOSS VARIANTS=$VARIANTS REP=$REP (5212: $second)"
  RATE=$RATE RTT=$RTT LOSS=$LOSS MODE=fqcodel NUSERS=1 NTUN=0 V6=0 "$RIG/rig.sh" up 2> "$OUT/rig.err" || { log "rig up failed: $(tail -1 "$OUT/rig.err")"; return 1; }
  rm -rf "$WORK"; mkdir -p "$WORK"; chmod 700 "$WORK"
  mkconf "$WORK" "$N" || { log "hs2 config failed"; return 1; }
  ip netns exec $E env HS2_NO_TUNE=1 $benv "$bp" run -c "$WORK/edge.json" > "$OUT/raw/edge-$BIN-$N-$LOSS-$REP.log" 2>&1 &
  pe=$!; PIDS+=($pe)
  sleep 0.5
  ip netns exec $X env HS2_NO_TUNE=1 $benv "$bp" run -c "$WORK/exit.json" > "$OUT/raw/exit-$BIN-$N-$LOSS-$REP.log" 2>&1 &
  px=$!; PIDS+=($px)
  n=$("$RIG/rig.sh" wait-links "$N" $((30 + N / 5)))
  [ "${n:-0}" -ge "$N" ] || { log "only ${n:-0} of $N links"; return 1; }
  # the server side in r-exit
  ip netns exec $X python3 "$D/tlstarget.py" "$KEYS/tls.crt" "$KEYS/tls.key" 127.0.0.1:4431 > /dev/null 2>&1 &
  PIDS+=($!)
  ip netns exec $X python3 "$D/xtraffic.py" server > /dev/null 2>&1 &
  PIDS+=($!)
  python3 "$D/mkxray.py" server "$KEYS/keys.env" "$WORK/xserver.json" --listen 127.0.0.1 --target 127.0.0.1:4431 \
    --cert "$KEYS/tls.crt" --key "$KEYS/tls.key" --access "$OUT/raw/xaccess-$BIN-$N-$LOSS-$REP.log" \
    --loglevel warning --inbounds "xr:5211,$second:5212" || { log "xray server config failed"; return 1; }
  ip netns exec $X "$XR" run -c "$WORK/xserver.json" > "$OUT/raw/xserver-$BIN-$N-$LOSS-$REP.log" 2>&1 &
  PIDS+=($!)
  sleep 1.5
  for v in ${VARIANTS//,/ }; do
    k=${KINDOF[$v]}; port=5311; [ "$k" = xr ] || port=5312
    id=$v-$BIN-N$N-L$LOSS-r$REP
    r=$OUT/raw/$id; rm -rf "$r"; mkdir -p "$r"
    python3 "$D/mkxray.py" client "$KEYS/keys.env" "$WORK/xclient-$v.json" --variant $v --addr 10.201.0.1 --port $port \
      --send-through 10.100.0.1 --loglevel warning --error "$r/xclient.log" || { log "client config $v failed"; continue; }
    stat_edge "$r/status-pre.txt"
    a0=$(wc -l < "$OUT/raw/xaccess-$BIN-$N-$LOSS-$REP.log" 2>/dev/null || echo 0)
    ip netns exec $E "$XR" run -c "$WORK/xclient-$v.json" > /dev/null 2>&1 &
    xc=$!
    sleep 1
    t0=$(date +%s.%N)
    ip netns exec $E python3 "$D/xtraffic.py" client --scenario lat --warm $WARM --dur $DUR --out "$r/flows.json" 2> "$r/flows.err" &
    pc=$!
    # during the download (3 s before its end), and after it
    sleep $(awk -v w=$WARM -v d=$DUR 'BEGIN{print w + d - 3}')
    stat_edge "$r/status-mid.txt"
    ip netns exec $E ss -tniH '( dport = :5311 or dport = :5312 )' > "$r/ss-user-mid.txt" 2>/dev/null
    ip netns exec $X ss -tinH state established '( dport = :2096 )' > "$r/ss-links-mid.txt" 2>/dev/null
    # where the bytes ahead of the game wait: the exit's sockets to the xray
    # server (Recv-Q: read by hs2 yet? Send-Q at xray's end) and the edge's
    # sockets to the xray client
    ip netns exec $X ss -tmniH '( dport = :5211 or dport = :5212 or sport = :5211 or sport = :5212 )' > "$r/ss-xsrv-mid.txt" 2>/dev/null
    ip netns exec $E ss -tmniH '( sport = :5311 or sport = :5312 )' > "$r/ss-xcli-mid.txt" 2>/dev/null
    wait $pc
    stat_edge "$r/status-end.txt"
    kill $xc 2>/dev/null; wait $xc 2>/dev/null
    tail -n +$((a0 + 1)) "$OUT/raw/xaccess-$BIN-$N-$LOSS-$REP.log" > "$r/xaccess.log" 2>/dev/null
    python3 "$D/xana.py" compose "$r" "$id" "BIN=$BIN N=$N LOSS=$LOSS VARIANT=$v REP=$REP" "$(sha256sum "$bp" | cut -c1-12)" "$benv" >> "$OUT/res.jsonl" \
      || log "compose $id failed"
    log "done $id: $(tail -1 "$OUT/res.jsonl" | python3 -c 'import json,sys; d=json.loads(sys.stdin.read()); g=d["game"]; print("game dl p50/p99 %s/%s ms, udp p99 %s, dl %s Mbit/s, %s" % (g["dl_p50"], g["dl_p99"], d["udpgame"]["dl_p99"], d["dl_mbit"], d.get("place", "")))' 2>/dev/null)"
    sleep 2
  done
  cleanup
}

tr -d '\r' < "$PLAN" | grep -v '^\s*\(#\|$\)' > "$OUT/plan.txt"
log "plan $PLAN: $(wc -l < "$OUT/plan.txt") lines"
while read -r line; do
  one_line "$line" < /dev/null
done < "$OUT/plan.txt"
log "done"

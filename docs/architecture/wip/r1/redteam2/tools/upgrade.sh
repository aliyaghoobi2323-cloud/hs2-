#!/bin/bash
# upgrade.sh OUT FROM_BIN TO_BIN ORDER GAP [PEERWAIT] — the R1 red team's
# (second pass, item 6) upgrade or rollback of both servers through the
# repository's hs2-safe-apply, timed. Run ONLY inside
# hs2-src/install/tests/sandbox.sh (it refuses otherwise): there no
# systemd, no real install path and no outside process is reachable.
#
# Two network namespaces as the field's reverse l3mtcp tunnel: ug-e (Iran,
# the edge: listens on 2096) and ug-x (Kharej, the exit: dials it), a veth
# with 37.5 ms each way (RTT 75, the field path), min_links = max_links = 8.
# An echo server behind the exit (socat, 127.0.0.1:5299) and a prober in the
# edge (ugprobe.py: a connection through user port 8443 every 100 ms, 16
# bytes echoed within 1 s). Both run FROM_BIN; once the probe passes, the
# switch: ORDER x-first (Kharej's safe-apply, then Iran's GAP s later),
# e-first (Iran, then Kharej), both (both at once), x-only (Kharej only: the
# second server never follows; safe-apply must put Kharej back). Each
# safe-apply runs as the operator would (detached), with fakesd.sh as
# systemctl and a ping inside the unit's namespace (pingshim.sh).
# Out: OUT/result.txt (the outage: first failed probe to the first passing
# one after the last restart; each safe-apply's verdict), events, probe log,
# both safe-apply logs, hs2 logs (scrubbed of the key).
set -u
[ "${HS2_SANDBOX:-}" = proven ] || { echo "upgrade.sh: run me inside sandbox.sh" >&2; exit 2; }
OUT=$1; FROM=$2; TO=$3; ORDER=$4; GAP=$5; PW=${6:-300}
T=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$T/../../../../../.." && pwd)
SA=$REPO/scripts/hs2-safe-apply
mkdir -p "$OUT"
W=$(mktemp -d /tmp/ug.XXXXXX)
export UGSD=$W/sd
mkdir -p "$UGSD/hs2-uge" "$UGSD/hs2-ugx" "$W/bin" "$W/stage-e" "$W/stage-x" "$W/shim"
E=ug-e; X=ug-x
PIDS=()
cleanup() {
	for u in hs2-uge hs2-ugx; do "$T/fakesd.sh" stop $u 2>/dev/null; done
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
	ip netns del $E 2>/dev/null; ip netns del $X 2>/dev/null
}
trap cleanup EXIT
ip netns add $E; ip netns add $X
ip link add uge0 netns $E type veth peer name ugx0 netns $X
ip -n $E addr add 192.168.81.1/24 dev uge0
ip -n $X addr add 192.168.81.2/24 dev ugx0
for n in $E $X; do ip -n $n link set lo up; done
ip -n $E link set uge0 up; ip -n $X link set ugx0 up
ip netns exec $E tc qdisc add dev uge0 root netem delay 37500us limit 100000
ip netns exec $X tc qdisc add dev ugx0 root netem delay 37500us limit 100000
KEY=$(openssl rand -hex 32)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout $W/key.pem -out $W/cert.pem -days 2 -subj /CN=lab.example.com 2>/dev/null
cat > $W/edge.json <<J
{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"192.168.81.1:2096",
 "iface":"ug0","local_cidr":"10.81.9.1/30","peer_ip":"10.81.9.2","mtu":1320,
 "shared_key":"$KEY","cert_file":"$W/cert.pem","key_file":"$W/key.pem","backend_addr":"builtin",
 "forward_ports":"8443","min_links":8,"max_links":8,"per_link":8}
J
cat > $W/exit.json <<J
{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"192.168.81.1:2096","sni":"lab.example.com",
 "iface":"ug0","local_cidr":"10.81.9.2/30","peer_ip":"10.81.9.1","mtu":1320,
 "shared_key":"$KEY","expose":"127.0.0.1:5299","min_links":8,"max_links":8,"per_link":8}
J
chmod 600 $W/*.json $W/key.pem
cp "$FROM" $W/bin/hs2-e; cp "$FROM" $W/bin/hs2-x
for u in e x; do
	d=$UGSD/hs2-ug$u
	[ $u = e ] && { echo $E > $d/ns; echo $W/edge.json > $d/cfg; } || { echo $X > $d/ns; echo $W/exit.json > $d/cfg; }
	echo $W/bin/hs2-$u > $d/bin; : > $d/env; : > $d/pid
	cp "$TO" $W/stage-$u/hs2
done
ln -s "$T/fakesd.sh" $W/shim/systemctl
cat > $W/shim/ping <<'S'
#!/bin/bash
exec ip netns exec "$UG_NS" /usr/bin/ping "$@"
S
chmod +x $W/shim/ping
ip netns exec $X socat TCP-LISTEN:5299,bind=127.0.0.1,fork,reuseaddr EXEC:cat > /dev/null 2>&1 &
PIDS+=($!)
"$T/fakesd.sh" start hs2-uge; sleep 0.5; "$T/fakesd.sh" start hs2-ugx
ip netns exec $E python3 "$T/ugprobe.py" > $W/probe.log 2>&1 &
PIDS+=($!)
# up on FROM: the probe passes for 3 s in a row (at most 120 s)
for i in $(seq 1 240); do
	sleep 0.5
	[ "$(tail -30 $W/probe.log | grep -c ' ok')" -ge 30 ] && break
done
t_up=$(date +%s.%N)
echo "up on $(basename "$FROM") after $(awk -v a="$(head -1 $W/probe.log | cut -d' ' -f1)" -v b="$t_up" 'BEGIN{printf "%.1f", b - a}') s" > $OUT/result.txt
sleep 3
sa() { # sa UNIT NS STAGE: hs2-safe-apply, detached, as the operator runs it
	( PATH=$W/shim:$PATH UG_NS=$2 HS2_SAFE_UNIT=$1 HS2_SAFE_BIN=$(cat $UGSD/$1/bin) HS2_SAFE_UNIT_DIR=$W/units \
	  HS2_SAFE_PEER_WAIT=$PW HS2_SAFE_LOG=$OUT/safe-apply-$1.log HS2_SAFE_LAST=$OUT/safe-apply-$1.last \
	  HS2_SAFE_BACKUPS=$W/backups bash "$SA" "$3" > /dev/null 2>&1 ) &
	PIDS+=($!)
	echo "$(date +%s.%N) safe-apply $1" >> $W/events
}
mkdir -p $W/units
t0=$(date +%s.%N)
case "$ORDER" in
x-first) sa hs2-ugx $X $W/stage-x; sleep "$GAP"; sa hs2-uge $E $W/stage-e ;;
e-first) sa hs2-uge $E $W/stage-e; sleep "$GAP"; sa hs2-ugx $X $W/stage-x ;;
both) sa hs2-ugx $X $W/stage-x; sa hs2-uge $E $W/stage-e ;;
x-only) sa hs2-ugx $X $W/stage-x ;;
esac
tlast=$(date +%s.%N)
# wait for both safe-apply runs to end (each: OK, REVERTED or REFUSED)
for i in $(seq 1 $((2 * PW + 200))); do
	sleep 1
	done_n=0
	for u in hs2-uge hs2-ugx; do grep -qE '^(OK|REVERTED|REFUSED)' $OUT/safe-apply-$u.last 2>/dev/null && done_n=$((done_n + 1)); done
	[ "$ORDER" = x-only ] && [ $done_n -ge 1 ] && break
	[ $done_n -ge 2 ] && break
done
sleep 5
python3 - "$W/probe.log" "$t0" "$tlast" >> $OUT/result.txt <<'PY'
import sys
log, t0, tlast = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
rows = [l.split() for l in open(log) if l.strip()]
rows = [(float(r[0]), r[1]) for r in rows if len(r) >= 2]
after = [r for r in rows if r[0] >= t0]
fail0 = next((t for t, s in after if s != "ok"), None)
if fail0 is None:
    print("no failed probe after the first switch")
    sys.exit()
back = next((t for t, s in after if s == "ok" and t > max(fail0, tlast)), None)
# the longest run of failures
longest, cur, start = 0.0, None, None
for t, s in after:
    if s != "ok":
        start = t if start is None else start
    elif start is not None:
        longest = max(longest, t - start); start = None
print("first failed probe %.1f s after the first safe-apply" % (fail0 - t0))
if back is None:
    print("never back (probe log ends %.1f s after the first safe-apply)" % (after[-1][0] - t0))
else:
    print("outage %.1f s (first failed probe to the first passing one after the last safe-apply); back %.1f s after the last safe-apply" % (back - fail0, back - tlast))
print("longest run of failed probes %.1f s; probes after the switch: %d ok, %d failed" % (longest, sum(1 for _, s in after if s == "ok"), sum(1 for _, s in after if s != "ok")))
PY
for u in hs2-uge hs2-ugx; do
	echo "$u safe-apply: $(cat $OUT/safe-apply-$u.last 2>/dev/null | sed "s#$W#<w>#g")" >> $OUT/result.txt
	sed -i "s#$W#<w>#g" $OUT/safe-apply-$u.log 2>/dev/null
	cp $UGSD/$u/events $OUT/events-$u.txt 2>/dev/null
	sed "s/$KEY/<key>/g; s#hs2://[^ \"]*#<hs2-link>#g; s#$W#<w>#g" $UGSD/$u/log > $OUT/$u.log
done
sed "s#$W#<w>#g" $W/events > $OUT/events.txt
cp $W/probe.log $OUT/probe.log
rm -rf "${W:?}"
cat $OUT/result.txt

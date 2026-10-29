#!/bin/bash
# fieldug.sh OUT FROM_BIN TO_BIN SCENARIO — the R1 review's (part C) check of
# the field runbook (RUNBOOK.fa.md): both servers switched through the
# repository's hs2-safe-apply exactly as the runbook says, timed. Built on the
# red team's upgrade.sh (redteam2/tools/), whose rig it keeps: run ONLY inside
# hs2-src/install/tests/sandbox.sh (it refuses otherwise), where no systemd,
# no real install path and no outside process is reachable.
#
# Two network namespaces as the field's reverse l3mtcp tunnel: ug-e (Iran, the
# edge: listens on 2096) and ug-x (Kharej, the exit: dials it), a veth with
# 37.5 ms each way (RTT 75, the field path), min_links = max_links = 6 (R1:
# 6 BULK + 4 QUIET). An echo server behind the exit, and a prober in the edge
# (redteam2/tools/ugprobe.py: a new connection through user port 8443 every
# 100 ms). Both run FROM_BIN; once the probe passes, the switch to TO_BIN:
#   e-first:GAP    Iran's safe-apply, Kharej's GAP s later (the runbook)
#   x-first:GAP    Kharej's first (a clock or a prep that put Kharej ahead)
#   both           both at once
#   xflap:GAP      as e-first, but Kharej's new binary "dies once" in its first
#                  minute (systemd's NRestarts 1): Kharej reverts, Iran stays —
#                  then the runbook's rule: Iran is put back as well (its
#                  safe-apply with FROM_BIN), timed from the moment Kharej's
#                  result says REVERTED
#   erefuse:GAP    as e-first, but Iran's safe-apply refuses (its stage holds a
#                  config hs2 check fails): Kharej goes alone
# HS2_SAFE_PEER_WAIT is the runbook's 120 s (PW to change it).
# Out: OUT/result.txt (the outage: first failed probe to the first passing one
# after the last safe-apply; each safe-apply's verdict; log lines a minute),
# events, probe log, both safe-apply logs, hs2 logs (scrubbed of the key),
# and an `hs2 status` of each end at the end.
set -u
[ "${HS2_SANDBOX:-}" = proven ] || { echo "fieldug.sh: run me inside sandbox.sh" >&2; exit 2; }
OUT=$1; FROM=$2; TO=$3; SCEN=$4; PW=${PW:-120}
T=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$T/../../../../../.." && pwd)
SA=$REPO/scripts/hs2-safe-apply
PROBE=$REPO/docs/architecture/wip/r1/redteam2/tools/ugprobe.py
ORDER=${SCEN%%:*}; GAP=${SCEN#*:}; [ "$GAP" = "$SCEN" ] && GAP=0
mkdir -p "$OUT"
W=$(mktemp -d /tmp/fug.XXXXXX)
export UGSD=$W/sd
mkdir -p "$UGSD/hs2-uge" "$UGSD/hs2-ugx" "$W/bin" "$W/stage-e" "$W/stage-x" "$W/stage-eback" "$W/shim" "$W/units"
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
 "forward_ports":"8443","min_links":6,"max_links":6,"per_link":8}
J
cat > $W/exit.json <<J
{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"192.168.81.1:2096","sni":"lab.example.com",
 "iface":"ug0","local_cidr":"10.81.9.2/30","peer_ip":"10.81.9.1","mtu":1320,
 "shared_key":"$KEY","expose":"127.0.0.1:5299","min_links":6,"max_links":6,"per_link":8}
J
chmod 600 $W/*.json $W/key.pem
cp "$FROM" $W/bin/hs2-e; cp "$FROM" $W/bin/hs2-x
for u in e x; do
	d=$UGSD/hs2-ug$u
	if [ $u = e ]; then echo $E > $d/ns; echo $W/edge.json > $d/cfg; else echo $X > $d/ns; echo $W/exit.json > $d/cfg; fi
	echo $W/bin/hs2-$u > $d/bin; : > $d/env; : > $d/pid
	cp "$TO" $W/stage-$u/hs2
done
cp "$FROM" $W/stage-eback/hs2
case "$ORDER" in
xflap) : > $UGSD/hs2-ugx/flap ;;
erefuse) sed 's/"mode":"dial"/"mode":"BAD"/' $W/edge.json > $W/stage-e/config.json ;;
esac
ln -s "$T/fakesd.sh" $W/shim/systemctl
cat > $W/shim/ping <<'S'
#!/bin/bash
exec ip netns exec "$UG_NS" /usr/bin/ping "$@"
S
chmod +x $W/shim/ping
ip netns exec $X socat TCP-LISTEN:5299,bind=127.0.0.1,fork,reuseaddr EXEC:cat > /dev/null 2>&1 &
PIDS+=($!)
"$T/fakesd.sh" start hs2-uge; sleep 0.5; "$T/fakesd.sh" start hs2-ugx
ip netns exec $E python3 "$PROBE" > $W/probe.log 2>&1 &
PIDS+=($!)
# up on FROM: the probe passes for 3 s in a row (at most 120 s)
for i in $(seq 1 240); do
	sleep 0.5
	[ "$(tail -30 $W/probe.log | grep -c ' ok')" -ge 30 ] && break
done
t_up=$(date +%s.%N)
echo "scenario $SCEN; HS2_SAFE_PEER_WAIT=$PW; from $(basename "$FROM") to $(basename "$TO")" > $OUT/result.txt
echo "up on $(basename "$FROM") after $(awk -v a="$(head -1 $W/probe.log | cut -d' ' -f1)" -v b="$t_up" 'BEGIN{printf "%.1f", b - a}') s" >> $OUT/result.txt
sleep 3
sa() { # sa UNIT NS STAGE TAG: hs2-safe-apply, detached, as the runbook runs it
	( PATH=$W/shim:$PATH UG_NS=$2 HS2_SAFE_UNIT=$1 HS2_SAFE_BIN=$(cat $UGSD/$1/bin) HS2_SAFE_UNIT_DIR=$W/units \
	  HS2_SAFE_PEER_WAIT=$PW HS2_SAFE_LOG=$OUT/safe-apply-$4.log HS2_SAFE_LAST=$OUT/safe-apply-$4.last \
	  HS2_SAFE_BACKUPS=$W/backups bash "$SA" "$3" > /dev/null 2>&1 ) &
	PIDS+=($!)
	echo "$(date +%s.%N) safe-apply $4" >> $W/events
}
done_tag() { grep -qE '^(OK|REVERTED|REFUSED)' $OUT/safe-apply-$1.last 2>/dev/null; }
t0=$(date +%s.%N)
case "$ORDER" in
e-first | xflap | erefuse) sa hs2-uge $E $W/stage-e e; sleep "$GAP"; sa hs2-ugx $X $W/stage-x x ;;
x-first) sa hs2-ugx $X $W/stage-x x; sleep "$GAP"; sa hs2-uge $E $W/stage-e e ;;
both) sa hs2-ugx $X $W/stage-x x; sa hs2-uge $E $W/stage-e e ;;
*) echo "unknown scenario $SCEN" >&2; exit 2 ;;
esac
tlast=$(date +%s.%N)
# wait for both safe-apply runs to end (each: OK, REVERTED or REFUSED)
rescue=""
for i in $(seq 1 $((3 * PW + 300))); do
	sleep 1
	# xflap, the runbook's rule: Kharej's log says REVERTING (its result line
	# comes only after its own wait for the tunnel) while Iran's says OK — put
	# Iran back too, at once
	if [ "$ORDER" = xflap ] && [ -z "$rescue" ] && grep -q REVERTING $OUT/safe-apply-x.log 2>/dev/null &&
		grep -q '^OK' $OUT/safe-apply-e.last 2>/dev/null; then
		rescue=$(date +%s.%N)
		echo "$rescue rescue: Iran back to $(basename "$FROM")" >> $W/events
		sa hs2-uge $E $W/stage-eback eback
	fi
	done_tag e && done_tag x && { [ -z "$rescue" ] || done_tag eback; } && break
done
sleep 5
# the status each end shows now (the runbook's "what to watch")
for u in e x; do
	cfg=$W/edge.json; [ $u = x ] && cfg=$W/exit.json
	ip netns exec ug-$u "$(cat $UGSD/hs2-ug$u/bin)" status -c "$cfg" > $OUT/status-$u.txt 2>&1
done
tl=$(date +%s.%N)
python3 - "$W/probe.log" "$t0" "$tlast" "${rescue:-0}" >> $OUT/result.txt <<'PY'
import sys
log, t0, tlast, rescue = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
rows = [l.split() for l in open(log) if l.strip()]
rows = [(float(r[0]), r[1]) for r in rows if len(r) >= 2]
after = [r for r in rows if r[0] >= t0]
fail0 = next((t for t, s in after if s != "ok"), None)
if fail0 is None:
    print("no failed probe after the first safe-apply")
    sys.exit()
ref = max(fail0, tlast, rescue)
back = next((t for t, s in after if s == "ok" and t > ref), None)
longest, start = 0.0, None
for t, s in after:
    if s != "ok":
        start = t if start is None else start
    elif start is not None:
        longest = max(longest, t - start); start = None
if start is not None:
    longest = max(longest, after[-1][0] - start)
print("first failed probe %.1f s after the first safe-apply" % (fail0 - t0))
if back is None:
    print("never back (probe log ends %.1f s after the first safe-apply)" % (after[-1][0] - t0))
else:
    print("outage %.1f s (first failed probe to the first passing one after the last safe-apply%s); back %.1f s after the last safe-apply" % (back - fail0, " and the rescue" if rescue else "", back - tlast))
if rescue:
    print("rescue (Iran put back) %.1f s after the first safe-apply; tunnel back %.1f s after the rescue" % (rescue - t0, (back - rescue) if back else -1))
print("longest run of failed probes %.1f s; probes after the switch: %d ok, %d failed" % (longest, sum(1 for _, s in after if s == "ok"), sum(1 for _, s in after if s != "ok")))
PY
for tag in e x eback; do
	[ -e $OUT/safe-apply-$tag.last ] || continue
	echo "safe-apply $tag: $(sed "s#$W#<w>#g" $OUT/safe-apply-$tag.last)" >> $OUT/result.txt
	sed -i "s#$W#<w>#g" $OUT/safe-apply-$tag.log 2>/dev/null
done
for u in hs2-uge hs2-ugx; do
	cp $UGSD/$u/events $OUT/events-$u.txt 2>/dev/null
	sed "s/$KEY/<key>/g; s#hs2://[^ \"]*#<hs2-link>#g; s#$W#<w>#g" $UGSD/$u/log > $OUT/$u.log
	# log volume: lines a minute over the whole run, without the start's tuning lines
	n=$(grep -vc 'tuning:' $OUT/$u.log)
	echo "$u log: $n lines (without tuning:) in $(awk -v a="$t_up" -v b="$tl" 'BEGIN{printf "%.0f", b - a + 5}') s" >> $OUT/result.txt
done
sed -i "s#$W#<w>#g" $OUT/status-e.txt $OUT/status-x.txt 2>/dev/null
sed "s#$W#<w>#g" $W/events > $OUT/events.txt
gzip -c $W/probe.log > $OUT/probe.log.gz
rm -rf "${W:?}"
cat $OUT/result.txt

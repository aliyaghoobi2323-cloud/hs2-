#!/bin/bash
# arm7.sh — R1 step 5: arm7 (GOARCH=arm GOARM=7). One command:
#   docs/architecture/wip/r1/merge-prep/step5/arm7.sh
# 1. the static binary (CGO_ENABLED=0, BUILD.md's flags), its sha256;
# 2. the test binaries of the packages R1 changed, built for arm7;
# 3. with qemu-user (qemu-arm or qemu-arm-static), each run under it with
#    -test.short: engine's R1 cores (ARM7_RUN), all of tlscarrier, core,
#    cmd/hs2, and smux's module; without it, 3 is skipped and said so.
# Result: $STEP5_OUT/arm7-<sha>-<time>.txt (exit 0 green, 1 a failure, 2 not
# run for lack of qemu-user, 3 the rig lock).
source "$(dirname "$0")/lib.sh"
OUT=$(step5_out arm7)
BIN=$OUTDIR/arm7-$SHA
mkdir -p "$BIN"
ARM7_RUN=${ARM7_RUN:-'Sched|Class|Lane|HostQ|Lowat|Gate|HostWait|SendCap|Budget|OWD|Hint|Events|QuietRecords|QuietCounts|FlowKey|Tun|Proxy|Syn|UserTag|Wire|Pin|Shape|Coalesce|Moved|MoveRule|UDPLate|Panel|LinkSndbuf'}
QEMU=${QEMU_ARM:-$(command -v qemu-arm-static || command -v qemu-arm || true)}
run() {
	set -o pipefail
	cd "$SRC" || exit 1
	export GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0
	echo "== 1. static binary"
	go build -trimpath -ldflags="-s -w" -o "$BIN/hs2-linux-armv7" ./cmd/hs2 || exit 1
	(cd "$BIN" && sha256sum hs2-linux-armv7 && ls -l hs2-linux-armv7)
	echo "== 2. test binaries for arm7"
	for p in engine tlscarrier core cmd/hs2; do
		go test -c -o "$BIN/$(echo $p | tr / -).test" "./$p" || exit 1
	done
	(cd third_party/smux && go test -c -o "$BIN/smux.test" .) || exit 1
	ls -l "$BIN"/*.test
	if [ -z "$QEMU" ]; then
		echo "== 3. NOT RUN: no qemu-user (qemu-arm or qemu-arm-static) on this host."
		echo "   Ubuntu: apt-get install qemu-user-static (14.7 MB download, 133 MB installed): needs the owner's approval."
		exit 2
	fi
	echo "== 3. under $QEMU ($($QEMU --version | head -1))"
	fail=0
	for t in engine tlscarrier core cmd-hs2 smux; do
		dir=$t; [ $t = cmd-hs2 ] && dir=cmd/hs2; [ $t = smux ] && dir=third_party/smux
		args="-test.short -test.count=1 -test.timeout=40m"
		[ $t = engine ] && args="$args -test.run $ARM7_RUN"
		echo "-- $t $args"
		t0=$(date +%s)
		(cd "$dir" && "$QEMU" "$BIN/$t.test" $args 2>&1 | tail -15) || fail=1
		echo "-- $t: $(( $(date +%s) - t0 )) s"
	done
	exit $fail
}
{
	step5_head "arm7 (GOARCH=arm GOARM=7)"
	step5_guard bash -c "$(declare -f run); BIN='$BIN' QEMU='$QEMU' ARM7_RUN='$ARM7_RUN' SRC='$SRC' run"
	rc=$?
	echo "== exit $rc"
} > "$OUT" 2>&1
echo "$OUT"; tail -3 "$OUT"; exit $rc

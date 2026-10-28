#!/bin/bash
# arm7.sh — R1 step 5: arm7 (GOARCH=arm GOARM=7). One command:
#   docs/architecture/wip/r1/merge-prep/step5/arm7.sh [engine tlscarrier core cmd-hs2 smux]
# (no argument: all five; one at a time fits a short gap of the rig lock)
# 1. the static binary (CGO_ENABLED=0, BUILD.md's flags), its sha256;
# 2. the test binaries of the packages R1 changed, built for arm7;
# 3. with qemu-user (qemu-arm or qemu-arm-static), each run under it with
#    -test.short: engine's R1 cores (ARM7_RUN), all of tlscarrier, core,
#    cmd/hs2, and smux's module; without it, 3 is skipped and said so.
# Result: $STEP5_OUT/arm7-<sha>-<time>.txt (exit 0 green, 1 a failure, 2 not
# run for lack of qemu-user, 3 the rig lock).
source "$(dirname "$0")/lib.sh"
OUT=$(step5_out arm7)
BIN=${OUT%.txt}-bin # a new directory each run
mkdir -p "$BIN"
ARM7_RUN=${ARM7_RUN:-'Sched|Class|Lane|HostQ|Lowat|Gate|HostWait|SendCap|Budget|OWD|Hint|Events|QuietRecords|QuietCounts|FlowKey|Tun|Proxy|Syn|UserTag|Wire|Pin|Shape|Coalesce|Moved|MoveRule|UDPLate|Panel|LinkSndbuf'}
# qemu-user: QEMU_ARM, else on PATH, else the official Ubuntu package's
# static binary unpacked (not installed: no binfmt, no system change) as
#   cd /root/rprep/tools && apt-get download qemu-user-static && dpkg -x qemu-user-static_*.deb qemu
QEMU=${QEMU_ARM:-$(command -v qemu-arm-static || command -v qemu-arm || ls /root/rprep/tools/qemu/usr/bin/qemu-arm-static 2>/dev/null || true)}
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
		echo "   the package qemu-user-static (Ubuntu 24.04, 14.7 MB) unpacked under /root/rprep/tools/qemu (see above), or QEMU_ARM=<path>"
		exit 2
	fi
	echo "== 3. under $QEMU ($($QEMU --version | head -1))"
	# the tests that call the kernel (*_linux_test.go: TCP_INFO, SIOCOUTQNSD,
	# ...) run apart: qemu-user hands a 32-bit guest 4 bytes of TCP_INFO and
	# no SIOCOUTQNSD, so there they meet the code's fallbacks, not the
	# kernel's calls (kernel.sh runs them natively); reported, not counted
	# and those that need what qemu-user does not give a guest: TestCPUMeter
	# reads its CPU time from /proc/self/stat, which qemu-user fakes (times
	# 0); TestLinkNoNetworkNoLog runs its own binary again (an ARM one, and no
	# binfmt here by design)
	ktests() {
		(cd "$SRC/$1" && { grep -ho '^func Test[A-Za-z0-9_]*' *_linux_test.go 2>/dev/null | sed 's/func //'
			[ "$1" = cmd/hs2 ] && printf '%s\n' TestCPUMeter TestLinkNoNetworkNoLog; } | grep -vx TestMain | sort -u | paste -sd'|')
	}
	summary() {
		echo "   pass $(grep -c '^--- PASS' "$1"), fail $(grep -c '^--- FAIL' "$1"), skip $(grep -c '^--- SKIP' "$1") (top level)"
		awk '/^=== RUN/{buf=""; next} /^--- FAIL: /{print "   FAIL " $3; printf "%s", buf; buf=""; next} {buf=buf "      " $0 "\n"}' "$1" | head -60
		awk '/^    [^ ].*\.go:[0-9]+: /{r=$0; sub(/^ +[^:]+:[0-9]+: /,"",r)} /^--- SKIP: /{print "   skip " $3 ": " r; r=""} /^=== RUN/{r=""}' "$1" | head -80
	}
	fail=0
	for t in $PKGS; do
		dir=$t; [ $t = cmd-hs2 ] && dir=cmd/hs2; [ $t = smux ] && dir=third_party/smux
		args="-test.short -test.count=1 -test.timeout=40m -test.v"
		[ $t = engine ] && args="$args -test.run $ARM7_RUN"
		kt=$(ktests "$dir")
		[ -n "$kt" ] && args="$args -test.skip ^($kt)\$"
		echo "-- $t: $args"
		t0=$(date +%s)
		(cd "$SRC/$dir" && "$QEMU" "$BIN/$t.test" $args > "$BIN/$t.log" 2>&1) || fail=1
		summary "$BIN/$t.log"
		echo "   $(tail -1 "$BIN/$t.log"); $(( $(date +%s) - t0 )) s"
		if [ -n "$kt" ]; then
			echo "-- $t, the kernel-facing tests and those qemu-user cannot serve, under emulation (not counted): ^($kt)\$"
			(cd "$SRC/$dir" && "$QEMU" "$BIN/$t.test" -test.count=1 -test.v -test.run "^($kt)\$" > "$BIN/$t-kernel.log" 2>&1)
			summary "$BIN/$t-kernel.log"
		fi
	done
	exit $fail
}
{
	step5_head "arm7 (GOARCH=arm GOARM=7): ${*:-engine tlscarrier core cmd-hs2 smux}"
	step5_guard bash -c "$(declare -f run); BIN='$BIN' QEMU='$QEMU' ARM7_RUN='$ARM7_RUN' SRC='$SRC' PKGS='${*:-engine tlscarrier core cmd-hs2 smux}' run"
	rc=$?
	echo "== exit $rc"
} > "$OUT" 2>&1
echo "$OUT"; tail -3 "$OUT"; exit $rc

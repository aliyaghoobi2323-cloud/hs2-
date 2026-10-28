#!/bin/bash
# fuzz.sh — R1 step 5: a go test -fuzz run per parser of peer data (REDESIGN
# §5.10), FUZZTIME each (60s). One command:
#   docs/architecture/wip/r1/merge-prep/step5/fuzz.sh [Target...]
# (no argument: every target below). A failure leaves its input under the
# package's testdata/fuzz/<Target>/ (go test's own): the result file names
# it. Result: $STEP5_OUT/fuzz-<sha>-<time>.txt (exit 0 green, 1 a target
# failed, 3 the rig lock).
source "$(dirname "$0")/lib.sh"
FUZZTIME=${FUZZTIME:-60s}
PAR=${FUZZ_PARALLEL:-4}
# module dir (from hs2-src), package, target, what it parses
TARGETS='
. ./engine FuzzFlowKey the TUN packet flow key: IPv4/IPv6 5-tuple, bounded extension headers (§4.3)
. ./engine FuzzFlowKeyRev the reverse flow key of a reply (pin by arrival, β2)
. ./engine FuzzOWDDecode the kindOWD one-way delay messages (owd.go)
. ./engine FuzzHintDecode the kindHint move hints from the exit (hint.go)
. ./engine FuzzProxyHeader the PROXY protocol v1/v2 header (proxyproto.go)
. ./tlscarrier FuzzParseClientAuth the v3 auth record: header under the MAC, level, caps (§4.1)
. ./tlscarrier FuzzParseTunDesc the 15-byte TUN descriptor (§4.3)
. ./cmd/hs2 FuzzDecodeLink the hs2:// link decoder (C15)
third_party/smux . FuzzSessionRecv the frames a peer sends: SYN payload (§4.2), group seq numbers, UPD
third_party/smux . FuzzGroupReassembly a group connection reassembled across its move (§4.7)
'
want=" $* "
run() {
	fail=0
	echo "$TARGETS" | while read -r mod pkg name what; do
		[ -n "$name" ] || continue
		[ "$want" = "  " ] || case "$want" in *" $name "*) ;; *) continue ;; esac
		echo "-- $name ($mod $pkg): $what"
		t0=$(date +%s)
		out=$(cd "$SRC/$mod" && go test -run '^$' -fuzz "^$name\$" -fuzztime "$FUZZTIME" -parallel "$PAR" "$pkg" 2>&1)
		rc=$?
		echo "$out" | grep -E '^(fuzz: elapsed|ok|FAIL|--- FAIL)|Failing input|failure while testing|panic:' | tail -6 | sed 's/^/   /'
		echo "   $name: rc $rc, $(( $(date +%s) - t0 )) s"
		[ $rc = 0 ] || echo "$name" >> "$FAILS"
	done
	git -C "$SRC" status --short -- '*testdata/fuzz*' | sed 's/^/   new corpus entry (a failing input): /'
	[ ! -s "$FAILS" ]
}
FAILS=$(mktemp)
OUT=$(step5_out fuzz)
{
	step5_head "fuzz, $FUZZTIME a target, -parallel $PAR"
	step5_guard bash -c "$(declare -f run); TARGETS='$TARGETS' want='$want' SRC='$SRC' FUZZTIME='$FUZZTIME' PAR='$PAR' FAILS='$FAILS' run"
	rc=$?
	[ -s "$FAILS" ] && echo "== failed: $(tr '\n' ' ' < "$FAILS")"
	echo "== exit $rc"
} > "$OUT" 2>&1
rm -f "$FAILS"
echo "$OUT"; tail -3 "$OUT"; exit $rc

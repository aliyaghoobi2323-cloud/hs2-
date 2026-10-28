#!/bin/bash
# kernel.sh — R1 step 5: the kernel matrix. One command:
#   docs/architecture/wip/r1/merge-prep/step5/kernel.sh
# This host (Ubuntu 24.04, kernel 6.8) is itself a VM without nested
# virtualization (no /dev/kvm, no vmx/svm), has no qemu and no 5.10 kernel,
# and fetching them needs the owner's approval; a TCG VM would also be far
# too slow to measure rig cells (README.fa.md here: why, and what instead).
# So, on whatever kernel it runs:
# 1. kprobe: every kernel interface the R1 cores use, with the kernel that
#    has it and the code's fallback without it (exit 1 if one is missing);
# 2. the tests that call the kernel (every Test in *_linux_test.go of
#    engine, tlscarrier, tun), run here;
# 3. a bundle in $STEP5_OUT/kernel-bundle-<sha>/ (kprobe, the test binaries,
#    run.sh) that runs 1 and 2 with one command on any other Linux of the
#    same architecture — a 5.10 host (Debian 11), or a VM once approved.
# With KERNEL_VM_CMD set (a command that runs its argument on a 5.10 guest
# with the bundle at the same path), 1 and 2 also run there.
# Result: $STEP5_OUT/kernel-<sha>-<time>.txt (exit 0 green, 1 a failure, 3
# the rig lock).
source "$(dirname "$0")/lib.sh"
OUT=$(step5_out kernel)
B=$OUTDIR/kernel-bundle-$SHA
run() {
	set -o pipefail
	rm -rf "$B" && mkdir -p "$B" || exit 1
	echo "== 1. kprobe"
	(cd "$STEP5_DIR/kprobe" && CGO_ENABLED=0 go build -o "$B/kprobe" main.go) || exit 1
	"$B/kprobe"; k=$?
	echo "== 2. the tests that call the kernel"
	cat > "$B/run.sh" <<'RUN'
#!/bin/bash
# run.sh: kprobe and the kernel-facing tests of this bundle, on this kernel.
cd "$(dirname "$0")" || exit 1
echo "== kernel $(uname -r)"
./kprobe; rc=$?
for t in *.test; do
	pkg=${t%.test}
	re=$(cat "$pkg.run")
	echo "-- $pkg: $re"
	(cd "data/$pkg" && "../../$t" -test.count=1 -test.timeout=20m -test.run "$re" 2>&1 | tail -12) || rc=1
done
exit $rc
RUN
	chmod +x "$B/run.sh"
	for p in engine tlscarrier tun; do
		names=$(cd "$SRC/$p" && grep -ho '^func Test[A-Za-z0-9_]*' *_linux_test.go 2>/dev/null | sed 's/func //' | sort -u | paste -sd'|')
		[ -n "$names" ] || continue
		(cd "$SRC" && CGO_ENABLED=0 go test -c -o "$B/$p.test" "./$p") || exit 1
		echo "^($names)\$" > "$B/$p.run"
		mkdir -p "$B/data/$p"
		[ -d "$SRC/$p/testdata" ] && cp -r "$SRC/$p/testdata" "$B/data/$p/"
	done
	"$B/run.sh" | sed 's/^/   /'; t=$?
	echo "== 3. bundle: $B ($(du -sh "$B" | cut -f1)); on a 5.10 host: copy the directory and run ./run.sh"
	if [ -n "${KERNEL_VM_CMD:-}" ]; then
		echo "== 4. on the guest: $KERNEL_VM_CMD"
		$KERNEL_VM_CMD "$B/run.sh" | sed 's/^/   /' || t=1
	else
		echo "== 4. no 5.10 guest here (KERNEL_VM_CMD unset): see README.fa.md"
	fi
	[ $k = 0 ] && [ $t = 0 ]
}
{
	step5_head "kernel matrix"
	step5_guard bash -c "$(declare -f run); B='$B' SRC='$SRC' STEP5_DIR='$STEP5_DIR' KERNEL_VM_CMD='${KERNEL_VM_CMD:-}' run"
	rc=$?
	echo "== exit $rc"
} > "$OUT" 2>&1
echo "$OUT"; tail -3 "$OUT"; exit $rc

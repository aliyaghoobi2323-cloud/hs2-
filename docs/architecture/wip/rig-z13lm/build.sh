#!/bin/bash
# build.sh TREE NAME [need|delay|fin|core]: copies the rig into TREE/engine and builds bin/NAME.test
# (at low priority, into a temporary file moved in place: a run in progress keeps its binary)
R=$(cd "$(dirname "$0")" && pwd); B=$R/../rig/bin
T=$1; N=$2; mkdir -p $B
cp $R/z5emu_test.go $R/z5rig_test.go $R/z5lm_test.go $T/engine/ || exit 1
[ "$3" = need ] && cp $R/z5knobs_need_test.go $T/engine/
[ "$3" = delay ] && cp $R/z5knobs_need_test.go $R/z5knobs_delay_test.go $T/engine/
[ "$3" = fin ] && cp $R/z5knobs_need_test.go $R/z5knobs_fin_test.go $T/engine/
[ "$3" = core ] && cp $R/z5knobs_need_test.go $R/z5knobs_core_test.go $T/engine/
(cd $T && nice -n 19 go test -p 1 -c -o $B/$N.test.new ./engine/) ; rc=$?
rm -f $T/engine/z5emu_test.go $T/engine/z5rig_test.go $T/engine/z5lm_test.go $T/engine/z5knobs_need_test.go $T/engine/z5knobs_delay_test.go $T/engine/z5knobs_fin_test.go $T/engine/z5knobs_core_test.go
[ $rc = 0 ] && mv $B/$N.test.new $B/$N.test
exit $rc

package engine

import (
	"errors"
	"testing"
)

// b271 Q5: a no-fresh-target death counts as lost only when the old rule
// ended the conn with the data (lost-copy reset, or every sub died).
func TestB271NoTargetOutcome(t *testing.T) {
	l0, s0 := subResendStat.noTgtLost.Load(), subResendStat.noTgtSurv.Load()
	for _, e := range []error{errMConnLost, errMConnSubsDied} {
		subNoTargetOutcome(e)
	}
	for _, e := range []error{nil, errMConnPeerDone, errors.New("x")} {
		subNoTargetOutcome(e)
	}
	if subResendStat.noTgtLost.Load()-l0 != 2 || subResendStat.noTgtSurv.Load()-s0 != 3 {
		t.Fatalf("lost +%d survived +%d, want 2 / 3", subResendStat.noTgtLost.Load()-l0, subResendStat.noTgtSurv.Load()-s0)
	}
}

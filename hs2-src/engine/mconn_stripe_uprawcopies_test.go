package engine

import (
	"strings"
	"testing"
	"time"
)

// ul197: HS2_MCONN_STRIPE_UP_RAW_COPIES gives an upload's raw upgrade open the
// SHARE_COPIES hint its later opens already have; off, or a download, or
// SHARE_COPIES off, or the exit: no change.
func TestStripeUpRawCopiesHint(t *testing.T) {
	oldC, oldD, oldU := stripeShareCopies.Load(), stripeDlCopies.Load(), stripeUpRawCopies.Load()
	t.Cleanup(func() { stripeShareCopies.Store(oldC); stripeDlCopies.Store(oldD); stripeUpRawCopies.Store(oldU) })
	stripeShareCopies.Store(true)
	stripeDlCopies.Store(false)
	m := &mConn{iran: true, stGrowUp: true}
	raw := func() bool { return stripeCopyUp(m.stripeRawCopyHintLocked(nil)) }

	stripeUpRawCopies.Store(false)
	if raw() {
		t.Fatal("key off: an upload's raw upgrade has the hint")
	}
	stripeUpRawCopies.Store(true)
	if !raw() {
		t.Fatal("key on: an upload's raw upgrade has no copies hint")
	}
	m.stGrowUp = false
	if raw() {
		t.Fatal("key on: a download's raw upgrade took the upload hint (DL_COPIES off)")
	}
	m.stGrowUp = true
	stripeShareCopies.Store(false)
	if raw() {
		t.Fatal("SHARE_COPIES off: hint")
	}
	stripeShareCopies.Store(true)
	m.iran = false
	if raw() {
		t.Fatal("exit side: hint")
	}
}

// ul197: the upload trace counts the gate's decisions and buckets a cut by
// how far its sample was over the threshold; nothing without the log key.
func TestStripeUpTraceGate(t *testing.T) {
	old := stripeLog
	t.Cleanup(func() { stripeLog = old })
	m := &mConn{iran: true}
	stripeLog = false
	m.trGateLocked(gwCut, 1, 5*time.Millisecond, 2*time.Millisecond)
	if m.stTrace.gw[gwCut] != 0 {
		t.Fatal("log off: counted")
	}
	stripeLog = true
	m.trGateLocked(gwCut, 2, 2500*time.Microsecond, 2*time.Millisecond)
	m.trGateLocked(gwCut, 1, 4*time.Millisecond, 2*time.Millisecond)
	m.trGateLocked(gwCut, 0, 7*time.Millisecond, 2*time.Millisecond)
	m.trGateLocked(gwCut, 0, 30*time.Millisecond, 2*time.Millisecond)
	m.trGateLocked(gwGrow, 3, 0, 2*time.Millisecond)
	tr := &m.stTrace
	if tr.gw[gwCut] != 4 || tr.gw[gwGrow] != 1 || tr.wMax != 3 || tr.cutQ != [4]uint16{1, 1, 1, 1} {
		t.Fatalf("trace %+v", tr)
	}
	if s := gwNames[gwFull]; !strings.Contains(s, "full") {
		t.Fatalf("gwNames %q", s)
	}
}

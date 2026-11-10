package engine

import (
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// awFakeSub records the adaptive-window switches a sub receives.
type awFakeSub struct {
	mSub
	on, off int
}

func (f *awFakeSub) SetAutoWindowOn()  { f.on++ }
func (f *awFakeSub) SetAutoWindowOff() { f.off++ }

// HS2_STRIPE_AW_SOLO: an Iran connection that may stripe but stays at width 1
// gets the adaptive window on its one sub after stripeAWSettle (and not
// before); off, it keeps the fixed window; a later widening turns it off.
func TestStripeAWSolo(t *testing.T) {
	oldAW, oldSolo := smuxAutoWin, stripeAWSolo
	t.Cleanup(func() { smuxAutoWin, stripeAWSolo = oldAW, oldSolo })
	smuxAutoWin = &smux.AutoWindow{Max: 32 << 20, UpdateDiv: 8, Budget: smux.NewWindowBudget(64 << 20)}

	run := func(solo bool) (*mConn, *awFakeSub) {
		stripeAWSolo = solo
		f := &awFakeSub{}
		m := &mConn{iran: true, stK: 8, subs: []*msub{{rw: f}}}
		t0 := time.Now()
		m.stripeAutoWinLocked(t0) // width 1 noted
		m.stripeAutoWinLocked(t0.Add(time.Second))
		if f.on != 0 {
			t.Fatalf("solo=%v: adaptive window before the settle time (on=%d)", solo, f.on)
		}
		m.stripeAutoWinLocked(t0.Add(stripeAWSettle + 100*time.Millisecond))
		return m, f
	}

	if _, f := run(false); f.on != 0 {
		t.Fatalf("key off: an unstriped sub got the adaptive window (on=%d)", f.on)
	}
	m, f := run(true)
	if f.on != 1 || !m.stAW.on {
		t.Fatalf("key on: unstriped sub after %v: on=%d state=%v, want the adaptive window", stripeAWSettle, f.on, m.stAW.on)
	}
	// the stripe widens: back to the fixed window until the new width settles
	f2 := &awFakeSub{}
	m.subs = append(m.subs, &msub{rw: f2, stripe: true})
	m.stN = 1
	m.stripeAutoWinLocked(time.Now().Add(stripeAWSettle + 200*time.Millisecond))
	if f.off != 1 || m.stAW.on {
		t.Fatalf("widened: off=%d state=%v, want the fixed window back", f.off, m.stAW.on)
	}
	// an ended connection never gets it
	stripeAWSolo = true
	fe := &awFakeSub{}
	me := &mConn{iran: true, stK: 8, ended: true, subs: []*msub{{rw: fe}}}
	t0 := time.Now()
	me.stripeAutoWinLocked(t0)
	me.stripeAutoWinLocked(t0.Add(2 * stripeAWSettle))
	if fe.on != 0 {
		t.Fatalf("ended connection got the adaptive window")
	}
}

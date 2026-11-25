package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// b465 HS2_LITE_HEDGE7=1 (Iran decides; default off; turns on HEDGE4 and
// HEDGE3 too, so set it on both sides as HEDGE4 is): F1i. A hedged lite conn
// is records from byte 0 (never raw), and the HS2_MCONN_STRIPE_DL_FAST
// download sampler (mconn_stripe_dlfast.go) ran only on a raw conn
// (rawW && rawBorn), so a big download on a hedged conn got its first stripe
// sub from the 1 s growth ticks 3.9-5.9 s in instead of ~1 s (F1h, cold
// ramp at 3 s -59 Mbit/s, 5/6 pairs worse).
//
// With the key the sampler also runs on a hedged records conn (Iran, h3)
// for stripeDlWindow from the conn's start (h7Born), once the conn is heavy
// (moved > hedge3Bytes: a light conn never gets the 200 ms ticks), and the
// manager tick runs at stripeDlStep while it does. Stripe subs on a records
// conn need no handshake: a records conn already grows stripe subs from its
// manager tick (openStripe(avoid, false)); the raw boundary handshake is only
// for leaving raw. No wire change, nothing at the exit.
var liteHedge7 = os.Getenv("HS2_LITE_HEDGE7") == "1" || liteHedge8Key || liteHedge10 // b466: HEDGE8 runs on HEDGE7

var hedge7Stat struct {
	on atomic.Uint64 // hedged records conns the download sampler ran on
}

// hedge7DlOnLocked (stripeDlOnLocked, a records conn, under m.mu, r.mu not
// held): the download sampler applies to this hedged conn now.
func (m *mConn) hedge7DlOnLocked(now time.Time) bool {
	if !liteHedge7 || !m.iran || !m.h3 || m.h7Born.IsZero() || now.Sub(m.h7Born) >= stripeDlWindow || m.mode != planeLight {
		return false
	}
	if !m.hedge3HeavyLocked() {
		return false
	}
	if m.stDlT.IsZero() {
		hedge7Stat.on.Add(1)
	}
	return true
}

// hedge7NextLocked (tick, records lite conn with stripe on): the next tick,
// every stripeDlStep while the sampler runs on a hedged conn.
func (m *mConn) hedge7NextLocked(now time.Time, d time.Duration) time.Duration {
	if !liteHedge7 || m.rawW.Load() {
		return d
	}
	return m.stripeDlNextLocked(now, d)
}

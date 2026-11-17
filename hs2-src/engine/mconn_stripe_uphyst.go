package engine

import (
	"sync/atomic"
	"time"
)

// ul197 gate hysteresis (HS2_MCONN_STRIPE_UPGATE_HYST, Iran, default off).
//
// Iran's split upload gate (gate74, mconn_stripe_split.go) cuts on the
// latest sample alone: excess over min(p10, 10 ms) above 67 % of the 3 ms
// target (2 ms). ul197's trace showed most cuts land <1-3 ms over that
// threshold, while the path's own srtt-minrtt jitter is 3.6-9.2 ms: the
// upload is cut on noise and 8/16 uploads with two subs never used them
// (wmax=0). With the key on, a cut needs either
//   - two consecutive samples over the threshold (a queue that holds), or
//   - one sample more than stripeUpHystFast (3 ms) over it (a real queue
//     building fast: the light users are protected at once, prod73 showed
//     a delayed cut costs them).
//
// A held sample neither cuts nor grows (gwBand). A previous over-sample
// older than stripeUpHystStale does not count (the gate was idle).
var stripeUpHyst atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_UPGATE_HYST

// stripeUpHystFast: excess over the cut threshold that cuts at once
// (HS2_MCONN_STRIPE_UPGATE_HYST_FAST_MS, default 3).
var stripeUpHystFast = envMsDefault("HS2_MCONN_STRIPE_UPGATE_HYST_FAST_MS", 3*time.Millisecond)

// stripeUpHystStale: a previous over-sample older than this is forgotten.
const stripeUpHystStale = 3 * stripeQEvery

// stripeUpHystHoldLocked (sender, under m.mu): true when this over-threshold
// cut sample is held (the first of a run and not far over). Every call
// records whether the sample was over.
func (m *mConn) stripeUpHystHoldLocked(now time.Time, cutQ, cutAt time.Duration) bool {
	over := cutQ > cutAt
	prev := m.stOverAt != (time.Time{}) && now.Sub(m.stOverAt) <= stripeUpHystStale
	if over {
		m.stOverAt = now
	} else {
		m.stOverAt = time.Time{}
	}
	if !over || prev || cutQ-cutAt > stripeUpHystFast {
		return false
	}
	if stripeLog && m.stTrace.held < 65535 {
		m.stTrace.held++
	}
	return true
}

package engine

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_FAST_EARLY_MS=N (Iran, with HS2_MCONN_STRIPE_FAST; 0/off
// by default, 50-500): a raw connection's first second is sampled every N ms
// instead of once (up57), so a fast upload starts its stripe (the raw →
// stripe upgrade) within ~2N ms instead of at the second mLitePoll tick (2 s).
//
// Why: the upgrade's handshake (mconn_raw.go) needs the exit to read the raw
// first sub up to the boundary before it answers; by 2 s a single upload has
// filled the raw link's send queue and window (several MB), and the up56 lab
// logged the handshake held ~4 s (`tick held hs=true`): seconds 1-5 of a
// single upload only +10 % with FAST. Started at ~0.4 s the raw queue is
// still a slow-start's worth, the handshake ends in about a round trip and
// the doubling chain (stFastMore) widens the stripe from there.
//
// An early sample grows only an upload (Iran sends more than it receives)
// carrying at least stripeFastRate (HS2_MCONN_STRIPE_FAST_MBIT, 30 Mbit/s):
// a page, a download's start or a light user's burst waits for the usual
// 1 s ticks and their rules (stripeHotTicks for a download). No wire change:
// the upgrade is the same handshake, earlier.
var stripeFastEarly atomic.Int64

func init() {
	stripeFastEarly.Store(int64(stripeEarlyFromEnv(os.Getenv("HS2_MCONN_STRIPE_FAST_EARLY_MS"))))
}

func stripeEarlyFromEnv(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(min(max(n, 50), 500)) * time.Millisecond
}

// stripeEarlyOn: the early window now (0: off).
func stripeEarlyOn() time.Duration {
	if !stripeFast.Load() || stripeK.Load() == 0 {
		return 0
	}
	return time.Duration(stripeFastEarly.Load())
}

// stripeEarlyFirst: a raw connection's first tick.
func stripeEarlyFirst() time.Duration {
	if e := stripeEarlyOn(); e > 0 {
		return e
	}
	return mLitePoll
}

// stripeEarlyLocked (rawTick, under m.mu): the early window for this tick (0:
// a normal tick) — only in the connection's first mLitePoll, before any stripe.
func (m *mConn) stripeEarlyLocked(now time.Time) time.Duration {
	e := stripeEarlyOn()
	if e == 0 || m.stK < 2 || m.stN > 0 || m.rawBorn.IsZero() || now.Sub(m.rawBorn) >= mLitePoll+e/2 {
		return 0
	}
	return e
}

// stripeEarlyNextLocked (rawTick, under m.mu): when the next raw tick runs.
// The early ticks end at the first mLitePoll; from there the usual period.
func (m *mConn) stripeEarlyNextLocked(now time.Time, early time.Duration) time.Duration {
	if early == 0 {
		return mLitePoll
	}
	if left := mLitePoll - now.Sub(m.rawBorn); left > early {
		return early
	} else if left > early/2 {
		return left
	}
	return mLitePoll
}

// stripeEarlyHotLocked: an early sample grows the stripe (an upload at
// stripeFastRate or more).
func stripeEarlyHotLocked(up bool, rate float64) bool {
	ok := up && rate >= float64(stripeFastRate.Load())
	if ok {
		stripeEarlyStat.Add(1)
	}
	return ok
}

// stripeEarlyStat: uploads whose stripe an early sample started.
var stripeEarlyStat atomic.Uint64

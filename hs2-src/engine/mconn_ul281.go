package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// HS2_UL281_SLOT (ul281, Iran = the edge that decides a growth; default ON
// since rel69l, =0 turns it off): a NEW fast upload whose first growth (raw -> stripe) finds
// every stripe slot (stripeMaxConns) taken may take one of the
// stripeDlFairExtra slots past the cap, as a fast download may
// (HS2_MCONN_STRIPE_DL_FAIR) — dlfair and dl241 refuse every upload.
//
// Why (lab2 ul281, prod-like 12 paths / ~130 links / 3000 crowd / 30 bg
// users, rel69k): the background streams (rate-limited uploads and downloads
// of 5-50 Mbit/s that grew to 2-8 subs) hold the 8 slots, the test upload's
// growth is refused every tick (uptrace "slot=9/10", subs=0) and it lives on
// its raw first link: one smux stream, whose adaptive window grows to 6-8 MiB
// in the first seconds (400-800 Mbit/s at 80 ms) and is then cut by the
// autowin queue rule back to the 2 MiB base (~200 Mbit/s) — the production
// shape [481, 741, 786, 307, 158, 214, 219, 216] (8 MiB then 2 MiB at
// ~85 ms). A slotted upload in the same runs reached 500-800.
//
// The extra slots stay bounded (stripeDlFairExtra past stripeMaxConns,
// process-wide, shared with dlfair/dl241); only an upload's first growth,
// at ul281MinRate or more, on a raw connection younger than dl241MaxAge (a
// new upload, not a background stream that lost its slot). Light users
// never reach the rate. No wire change. Off (=0): rel69k behaviour.
// lab2 ul281d, 10 interleaved off/on pairs: upload 229 -> 327 Mbit/s
// (+98, 95 % CI [+72, +123], 10/10), light p99 diffs ~0, 0 fails.
var ul281Slot atomic.Bool

func init() { ul281Slot.Store(envBoolDefault("HS2_UL281_SLOT", true)) }

// ul281MinRate: bytes/s an upload's raw sample must show to take an extra
// slot (80 Mbit/s, as stripeDlFastRate).
const ul281MinRate = 80e6 / 8

// ul281Stat: diagnostics.
var ul281Stat struct {
	taken, refused atomic.Uint64
}

// ul281SlotLocked (a first growth found every stripe slot taken: c is the
// count with this one).
func (m *mConn) ul281SlotLocked(c int32, up bool, rate float64, now time.Time) bool {
	return m.ul281SlotOK(c, up, rate, now, ul281Slot.Load())
}

func (m *mConn) ul281SlotOK(c int32, up bool, rate float64, now time.Time, on bool) bool {
	if !up || !m.iran || m.rawBorn.IsZero() || rate < ul281MinRate {
		return false
	}
	age := now.Sub(m.rawBorn)
	if age >= dl241MaxAge {
		return false
	}
	if !on || c > stripeMaxConns+stripeDlFairExtra {
		if n := ul281Stat.refused.Add(1); stripeLog && (n <= 50 || n%500 == 0) {
			log.Printf("mconn %x: ul281: new upload refused a stripe slot (age %v, %.0f Mbit/s, slot %d, key %v, refused %d)", m.id&0xffffff,
				age.Round(time.Millisecond), rate*8/1e6, c, on, n)
		}
		return false
	}
	n := ul281Stat.taken.Add(1)
	if n <= 50 || n%500 == 0 {
		log.Printf("mconn %x: ul281: new upload took stripe slot %d past the cap of %d (age %v, %.0f Mbit/s, taken %d)", m.id&0xffffff,
			c, stripeMaxConns, age.Round(time.Millisecond), rate*8/1e6, n)
	}
	return true
}

// UL281Stats: new uploads given an extra slot, and fast new uploads refused one.
func UL281Stats() (taken, refused uint64) {
	return ul281Stat.taken.Load(), ul281Stat.refused.Load()
}

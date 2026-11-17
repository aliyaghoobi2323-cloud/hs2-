package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// HS2_DL241_SLOWSLOT (dl241, Iran = the edge that decides a growth; default
// OFF, =1 turns it on): a NEW download that passed the usual hot rule
// (stripeHotTicks 1 s samples in a row at the growth rate) but is under
// stripeDlFastRate may also take one of the stripeDlFairExtra slots past
// stripeMaxConns, as a fast download may (HS2_MCONN_STRIPE_DL_FAIR).
//
// Why (lab2 dl240 B1 t260, per second 5, 12, 13, 21, 23, 27, 48, 58, 74, 105,
// 79, 261): the download's first link was slow (a crowded / lossy link), so
// the fast rule (>= 80 Mbit/s) never applied; the usual rule passed its hot
// ticks after ~4 s, but all 8 stripe slots were held by background streams
// (trSlot, the tick refused every second); only at 101 Mbit/s, 10 s in, did
// dlfair's extra slot let it stripe ("stripe slot 9 past the cap of 8 (101
// Mbit/s, extra 5)") — the slowest download is exactly the one that needs
// the other links most and the one the extra slots refused.
//
// With the key, the extra slot also goes to a download (never an upload) whose
// raw connection is younger than dl241MaxAge (a new download, not a
// background stream that lost its slot) and that passed the hot ticks. The
// extra slots stay bounded (stripeDlFairExtra, process-wide). No wire change.
var dl241SlowSlot atomic.Bool

func init() { dl241SlowSlot.Store(envBoolDefault("HS2_DL241_SLOWSLOT", false)) }

// dl241MaxAge: a raw connection's age up to which it counts as a new download.
const dl241MaxAge = 20 * time.Second

// dl241Stat: diagnostics.
var dl241Stat struct {
	slow, refused atomic.Uint64
}

// dl241SlowSlotLocked (a first growth found every stripe slot taken and the
// fast extra refused it: c is the count with this one).
func (m *mConn) dl241SlowSlotLocked(c int32, up bool, rate float64, now time.Time) bool {
	if up || !m.iran || m.rawBorn.IsZero() {
		return false
	}
	age := now.Sub(m.rawBorn)
	ok := dl241SlowSlot.Load() && stripeDlFair.Load() && c <= stripeMaxConns+stripeDlFairExtra &&
		m.stHot >= int(stripeHotTicks.Load()) && age < dl241MaxAge
	if !ok {
		if age < dl241MaxAge && m.stHot >= int(stripeHotTicks.Load()) {
			if n := dl241Stat.refused.Add(1); stripeLog && (n <= 50 || n%500 == 0) {
				log.Printf("mconn %x: dl241: new download refused a stripe slot (age %v, %.0f Mbit/s, slot %d, key %v, refused %d)", m.id&0xffffff,
					age.Round(time.Millisecond), rate*8/1e6, c, dl241SlowSlot.Load(), n)
			}
		}
		return false
	}
	n := dl241Stat.slow.Add(1)
	if n <= 50 || n%500 == 0 {
		log.Printf("mconn %x: dl241: slow new download took stripe slot %d past the cap of %d (age %v, %.0f Mbit/s, slow %d)", m.id&0xffffff,
			c, stripeMaxConns, age.Round(time.Millisecond), rate*8/1e6, n)
	}
	return true
}

// DL241Stats: slow new downloads given an extra slot, and new downloads
// refused one.
func DL241Stats() (slow, refused uint64) {
	return dl241Stat.slow.Load(), dl241Stat.refused.Load()
}

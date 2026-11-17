package engine

import (
	"testing"
	"time"
)

// dl241 (HS2_DL241_SLOWSLOT, Iran): a new download that passed the hot ticks
// under stripeDlFastRate takes an extra stripe slot with the key; never an
// upload, an old connection, one short of the hot ticks, past the extra
// slots, or with the key off.
func TestDL241SlowSlot(t *testing.T) {
	oldK, oldF, oldH := dl241SlowSlot.Load(), stripeDlFair.Load(), stripeHotTicks.Load()
	t.Cleanup(func() { dl241SlowSlot.Store(oldK); stripeDlFair.Store(oldF); stripeHotTicks.Store(oldH) })
	stripeDlFair.Store(true)
	stripeHotTicks.Store(4)
	now := time.Now()
	m := &mConn{iran: true}
	m.rawBorn = now.Add(-5 * time.Second)
	m.stHot = 4
	rate := 20e6 / 8
	c := int32(stripeMaxConns + 1)

	dl241SlowSlot.Store(false)
	if m.dl241SlowSlotLocked(c, false, rate, now) {
		t.Fatal("key off: want refused")
	}
	dl241SlowSlot.Store(true)
	if !m.dl241SlowSlotLocked(c, false, rate, now) {
		t.Fatal("key on, new hot download: want the slot")
	}
	if m.dl241SlowSlotLocked(c, true, rate, now) {
		t.Fatal("upload: want refused")
	}
	if m.dl241SlowSlotLocked(stripeMaxConns+stripeDlFairExtra+1, false, rate, now) {
		t.Fatal("past the extra slots: want refused")
	}
	m.stHot = 3
	if m.dl241SlowSlotLocked(c, false, rate, now) {
		t.Fatal("short of the hot ticks: want refused")
	}
	m.stHot = 4
	m.rawBorn = now.Add(-dl241MaxAge - time.Second)
	if m.dl241SlowSlotLocked(c, false, rate, now) {
		t.Fatal("old connection: want refused")
	}
	x := &mConn{iran: false, rawBorn: now}
	x.stHot = 4
	if x.dl241SlowSlotLocked(c, false, rate, now) {
		t.Fatal("exit side: want refused")
	}
}

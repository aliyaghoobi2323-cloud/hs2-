package engine

import (
	"testing"
	"time"
)

// ul281 (HS2_UL281_SLOT, Iran): a new fast upload whose first growth found
// every stripe slot taken takes an extra slot with the key; never a download
// (dlfair/dl241's), a slow or old upload, past the extra slots, the exit
// side, or with the key off.
func TestUL281Slot(t *testing.T) {
	old := ul281Slot.Load()
	t.Cleanup(func() { ul281Slot.Store(old) })
	now := time.Now()
	m := &mConn{iran: true}
	m.rawBorn = now.Add(-2 * time.Second)
	fast := 200e6 / 8
	c := int32(stripeMaxConns + 1)

	ul281Slot.Store(false)
	if m.ul281SlotLocked(c, true, fast, now) {
		t.Fatal("key off: want refused")
	}
	ul281Slot.Store(true)
	if !m.ul281SlotLocked(c, true, fast, now) {
		t.Fatal("key on, new fast upload: want the slot")
	}
	if !m.ul281SlotLocked(stripeMaxConns+stripeDlFairExtra, true, fast, now) {
		t.Fatal("last extra slot: want the slot")
	}
	if m.ul281SlotLocked(c, false, fast, now) {
		t.Fatal("download: want refused (dlfair/dl241 decide those)")
	}
	if m.ul281SlotLocked(c, true, 40e6/8, now) {
		t.Fatal("slow upload: want refused")
	}
	if m.ul281SlotLocked(stripeMaxConns+stripeDlFairExtra+1, true, fast, now) {
		t.Fatal("past the extra slots: want refused")
	}
	m.rawBorn = now.Add(-dl241MaxAge - time.Second)
	if m.ul281SlotLocked(c, true, fast, now) {
		t.Fatal("old connection (background stream): want refused")
	}
	if (&mConn{iran: true}).ul281SlotLocked(c, true, fast, now) {
		t.Fatal("no raw birth: want refused")
	}
	x := &mConn{iran: false, rawBorn: now}
	if x.ul281SlotLocked(c, true, fast, now) {
		t.Fatal("exit side: want refused")
	}
}

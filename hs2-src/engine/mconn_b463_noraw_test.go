package engine

import "testing"

// HS2_B463_NORAW: the opener asks for records (no raw first sub); off: as before.
func TestB463NoRaw(t *testing.T) {
	old, oldH := noRaw463, liteHedge
	defer func() { noRaw463, liteHedge = old, oldH }()
	liteHedge = false
	noRaw463 = false
	if hedgeWantRecords() {
		t.Fatal("records wanted with both keys off")
	}
	noRaw463 = true
	if !hedgeWantRecords() {
		t.Fatal("NORAW on: records not wanted")
	}
}

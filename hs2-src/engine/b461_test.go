package engine

import (
	"errors"
	"strings"
	"testing"
)

// The b461 end counter counts lite conns with the hedge keys off (arm A),
// splits error / RST / heavy, and ignores non-lite conns and clean closes.
func TestB461EndsCounted(t *testing.T) {
	setHedge4(t, false)
	liteHedge3, liteHedge = false, false
	e0, r0, h0, n0 := b461Stat.errEnds.Load(), b461Stat.rstEnds.Load(), b461Stat.heavyErr.Load(), b461Stat.ends.Load()
	mk := func(lite bool) *mConn {
		mc := newMConnExit(61)
		mc.mu.Lock()
		mc.lite = lite
		mc.mu.Unlock()
		return mc
	}
	a := mk(true)
	a.end(errMConnClosed, false) // clean
	b := mk(true)
	b.mu.Lock()
	b.sendOff = hedge3Bytes + 1
	b.mu.Unlock()
	b.end(errors.New("x"), true) // heavy, error, rst
	c := mk(false)
	c.end(errors.New("y"), true) // not lite: not counted
	if b461Stat.ends.Load() != n0+2 || b461Stat.errEnds.Load() != e0+1 || b461Stat.rstEnds.Load() != r0+1 || b461Stat.heavyErr.Load() != h0+1 {
		t.Fatalf("ends %d err %d rst %d heavy %d", b461Stat.ends.Load()-n0, b461Stat.errEnds.Load()-e0, b461Stat.rstEnds.Load()-r0, b461Stat.heavyErr.Load()-h0)
	}
	if s := b461ErrLine(); !strings.Contains(s, "x/rst=1") || strings.Contains(s, "; y/rst=") || strings.HasPrefix(s, "y/rst=") {
		t.Fatalf("kinds %q", s)
	}
}

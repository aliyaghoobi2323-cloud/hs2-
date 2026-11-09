package smux

import (
	"testing"
	"time"
)

// relBase: the lower decile of the recent excesses, capped, and forgetting
// samples older than relSpan.
func TestAutoWinRelBase(t *testing.T) {
	b := NewWindowBudget(64 << 20)
	now := time.Now()
	for i := 0; i < 100; i++ {
		b.relBase(time.Duration(i)*100*time.Microsecond, now)
	}
	if got := b.relBase(0, now.Add(2*time.Second)); got < 900*time.Microsecond || got > 1100*time.Microsecond {
		t.Fatalf("base %v, want ~1ms (lower decile)", got)
	}
	for i := 0; i < relN; i++ { // fills the ring
		b.relBase(50*time.Millisecond, now.Add(3*time.Second))
	}
	if got := b.relBase(50*time.Millisecond, now.Add(5*time.Second)); got != relBaseCap {
		t.Fatalf("base %v, want the cap %v", got, relBaseCap)
	}
	if got := b.relBase(0, now.Add(40*time.Second)); got != 0 {
		t.Fatalf("base %v after relSpan, want 0 (too few fresh samples)", got)
	}
	if b.RelBase() != 0 {
		t.Fatalf("RelBase %v", b.RelBase())
	}
}

// shareCap: Share split among the heavy grown streams (a stream not grown
// yet counts itself), never below the base; 0 with Share off.
func TestAutoWinShareCap(t *testing.T) {
	b := NewWindowBudget(512 << 20)
	cfg := &AutoWindow{Max: 32 << 20, Budget: b}
	a := newStreamAW(cfg, 2<<20)
	if c := a.shareCap(); c != 0 {
		t.Fatalf("share off: cap %d", c)
	}
	cfg.Share = 32 << 20
	if c := a.shareCap(); c != 32<<20 {
		t.Fatalf("alone: cap %d, want 32 MiB", c)
	}
	b.heavyN.Store(7) // seven others heavy, this one at its base
	if c := a.shareCap(); c != 4<<20 {
		t.Fatalf("8 heavy: cap %d, want 4 MiB", c)
	}
	a.grown.Store(true) // now counted among the seven
	if c := a.shareCap(); c != (32<<20)/7 {
		t.Fatalf("7 heavy: cap %d", c)
	}
	b.heavyN.Store(40)
	if c := a.shareCap(); c != 2<<20 {
		t.Fatalf("many heavy: cap %d, want the base", c)
	}
}

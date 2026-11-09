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
	for i := 0; i < 100; i++ {
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

package engine

import (
	"testing"
	"time"
)

// پایهٔ هر لینک لرزش ثابت همان لینک را کم می‌کند و سقف دارد.
func TestStripeLinkBase(t *testing.T) {
	var lb linkBase
	if _, ok := lb.excess(time.Millisecond); ok {
		t.Fatal("base before samples")
	}
	for i := 0; i < stripeBaseMin; i++ {
		lb.note(time.Duration(2+i%3)*time.Millisecond, 4*time.Millisecond)
	}
	e, ok := lb.excess(5 * time.Millisecond)
	if !ok || e != 3*time.Millisecond {
		t.Fatalf("excess %v %v, want 3ms", e, ok)
	}
	var hi linkBase
	for i := 0; i < stripeBaseN+5; i++ {
		hi.note(10*time.Millisecond, 4*time.Millisecond)
	}
	if e, _ := hi.excess(10 * time.Millisecond); e != 6*time.Millisecond {
		t.Fatalf("capped excess %v, want 6ms", e)
	}
}

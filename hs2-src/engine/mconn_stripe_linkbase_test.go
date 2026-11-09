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

// پلهٔ مسیر: صفی که ۲ ثانیه بالای هدف ماند پذیرفته می‌شود و با پایین آمدن صف کنار می‌رود.
func TestStripeStepAdjust(t *testing.T) {
	old := stripeStepMax
	stripeStepMax = 10 * time.Millisecond
	defer func() { stripeStepMax = old }()
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
	tg := 3 * time.Millisecond
	t0 := time.Unix(1000, 0)
	if q := p.stepAdjust(t0, 8*time.Millisecond, tg); q != 8*time.Millisecond {
		t.Fatalf("first %v", q)
	}
	if q := p.stepAdjust(t0.Add(time.Second), 8*time.Millisecond, tg); q != 8*time.Millisecond {
		t.Fatalf("before wait %v", q)
	}
	if q := p.stepAdjust(t0.Add(2100*time.Millisecond), 8*time.Millisecond, tg); q != 0 {
		t.Fatalf("after wait %v, want 0", q)
	}
	if q := p.stepAdjust(t0.Add(2200*time.Millisecond), 10*time.Millisecond, tg); q != 2*time.Millisecond {
		t.Fatalf("over step %v", q)
	}
	if q := p.stepAdjust(t0.Add(2300*time.Millisecond), 0, tg); q != 0 || p.step.b != 0 {
		t.Fatalf("down %v b=%v", q, p.step.b)
	}
	// cap
	p.stepAdjust(t0.Add(3*time.Second), 30*time.Millisecond, tg)
	if q := p.stepAdjust(t0.Add(5100*time.Millisecond), 30*time.Millisecond, tg); q != 20*time.Millisecond {
		t.Fatalf("capped %v", q)
	}
}

// مهلت پرش: پرش یک‌باره تا مهلت برش نمی‌خورد؛ بالا آمدن آرام برش عادی.
func TestStripeJumpHold(t *testing.T) {
	old := stripeJump
	stripeJump = 1500 * time.Millisecond
	defer func() { stripeJump = old }()
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
	tg := 3 * time.Millisecond
	t0 := time.Unix(1000, 0)
	if p.jumpHold(t0, 0, tg) {
		t.Fatal("low q held")
	}
	if !p.jumpHold(t0.Add(100*time.Millisecond), 9*time.Millisecond, tg) {
		t.Fatal("jump not held")
	}
	if !p.jumpHold(t0.Add(time.Second), 9*time.Millisecond, tg) {
		t.Fatal("jump not held within grace")
	}
	if p.jumpHold(t0.Add(1700*time.Millisecond), 9*time.Millisecond, tg) {
		t.Fatal("held after grace")
	}
	p.jumpHold(t0.Add(2*time.Second), time.Millisecond, tg)
	p.jumpHold(t0.Add(2100*time.Millisecond), 4*time.Millisecond, tg) // slow rise: under 2x
	if p.jumpHold(t0.Add(2200*time.Millisecond), 9*time.Millisecond, tg) {
		t.Fatal("slow rise held")
	}
}

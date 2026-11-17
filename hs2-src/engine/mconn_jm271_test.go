package engine

import (
	"testing"
	"time"
)

type jm271Rec struct {
	buf []byte
	dl  bool
}

func (r *jm271Rec) SetWriteDeadline(time.Time) error {
	r.dl = true
	return nil
}

func (r *jm271Rec) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	return len(p), nil
}

// jm271: a missed join gets one RST record; key off: nothing; over the
// bucket: nothing (counted as limited); the bucket refills at its rate.
func TestJM271MissedJoinRst(t *testing.T) {
	was := jm271On.Load()
	oldLim := jm271Limit
	t.Cleanup(func() { jm271On.Store(was); jm271Limit = oldLim })

	jm271On.Store(true)
	jm271Limit = &jm271Bucket{rate: 1000, burst: 3}
	s0, l0 := jm271Stat.sent.Load(), jm271Stat.limited.Load()
	for i := 0; i < 5; i++ {
		var r jm271Rec
		jm271MissedJoin(&r)
		want := i < 3
		if got := len(r.buf) == 1 && r.buf[0] == mRecRst && r.dl; got != want {
			t.Fatalf("join %d: rst %v (%x), want %v", i, got, r.buf, want)
		}
	}
	if jm271Stat.sent.Load()-s0 != 3 || jm271Stat.limited.Load()-l0 != 2 {
		t.Fatalf("sent +%d limited +%d, want 3 / 2", jm271Stat.sent.Load()-s0, jm271Stat.limited.Load()-l0)
	}

	jm271On.Store(false)
	var r jm271Rec
	jm271MissedJoin(&r)
	if len(r.buf) != 0 {
		t.Fatal("key off wrote an RST")
	}
}

func TestJM271Bucket(t *testing.T) {
	b := &jm271Bucket{rate: 10, burst: 2}
	now := time.Now()
	if !b.allow(now) || !b.allow(now) || b.allow(now) {
		t.Fatal("burst of 2 not enforced")
	}
	if b.allow(now.Add(50 * time.Millisecond)) {
		t.Fatal("refilled too fast")
	}
	if !b.allow(now.Add(150 * time.Millisecond)) {
		t.Fatal("not refilled after 1/rate")
	}
	if !b.allow(now.Add(time.Hour)) || !b.allow(now.Add(time.Hour)) || b.allow(now.Add(time.Hour)) {
		t.Fatal("refill not capped at the burst")
	}
}

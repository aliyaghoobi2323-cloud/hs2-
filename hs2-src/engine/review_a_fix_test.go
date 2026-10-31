package engine

import (
	"math/rand/v2"
	"testing"
	"time"
)

// Random pushes, frames built and written, drops, ticks that forget idle
// flows (the clock moved on), and a close: after each, the sets of what has
// packets queued agree with the queues (A2's fix).
func TestReviewATunSetsRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 20; round++ {
		sq := newTunSched(nil)
		var taken []*tunFlow
		skew := int64(0) // the tick's clock past now
		for op := 0; op < 4000; op++ {
			switch k := r.IntN(100); {
			case k < 70:
				b := make([]byte, 1+r.IntN(1400))
				sq.push(&b, uint32(1+r.IntN(40)), uint32(1+r.IntN(300)), r.IntN(10) == 0)
			case k < 85:
				sq.mu.Lock()
				f := sq.readyOut()
				if f != nil {
					sq.build(f, 1+r.IntN(20000), ctrlNow()+skew)
					f.busy = true
					taken = append(taken, f)
				}
				sq.mu.Unlock()
			case k < 93:
				if len(taken) > 0 {
					i := r.IntN(len(taken))
					f := taken[i]
					taken = append(taken[:i], taken[i+1:]...)
					if r.IntN(4) == 0 {
						sq.held(f)
					} else {
						f.written(nil)
					}
				}
			case k < 97:
				sq.mu.Lock()
				for _, f := range sq.flows {
					if r.IntN(3) == 0 {
						sq.dropHead(f)
					}
				}
				sq.mu.Unlock()
			default:
				skew += int64(time.Duration(r.IntN(3000)) * time.Millisecond)
				sq.tick(ctrlNow() + skew)
			}
			tunQueuedSetsAgree(t, sq)
		}
		sq.close()
		tunQueuedSetsAgree(t, sq)
		if len(sq.busyU) != 0 || sq.pkts != 0 {
			t.Fatalf("closed: %d users queued, %d packets", len(sq.busyU), sq.pkts)
		}
	}
}

// The R1 review, part A, A2's fix: the sets of what has packets queued
// (tunUser.busy, tunSched.busyU) hold exactly the flows and users whose
// queues are not empty.
func init() {
	tunQueuedSetsAgree = func(t *testing.T, sq *tunSched) {
		t.Helper()
		sq.mu.Lock()
		defer sq.mu.Unlock()
		pkts := 0
		for _, u := range sq.users {
			n, busy := 0, 0
			for f := range u.flows {
				n += f.n()
				_, in := u.busy[f]
				if in != (f.n() > 0) {
					t.Fatalf("user %d flow %d: %d queued, in busy %v", u.tag, f.key, f.n(), in)
				}
				if f.n() > 0 {
					busy++
				}
			}
			if n != u.pkts || busy != len(u.busy) {
				t.Fatalf("user %d: pkts %d (queues %d), busy %d (flows queued %d)", u.tag, u.pkts, n, len(u.busy), busy)
			}
			if _, in := sq.busyU[u]; in != (u.pkts > 0) {
				t.Fatalf("user %d: %d queued, in busyU %v", u.tag, u.pkts, in)
			}
			pkts += n
		}
		if pkts != sq.pkts {
			t.Fatalf("link: pkts %d, queues %d", sq.pkts, pkts)
		}
		for u := range sq.busyU {
			if sq.users[u.tag] != u {
				t.Fatalf("busyU holds user %d the link forgot", u.tag)
			}
		}
	}
}

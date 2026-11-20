//go:build linux

package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// bootRig records the review issue 4 hooks of a rig.
type bootRig struct {
	mu                        sync.Mutex
	pause, resume, on, offCnt int
}

func (b *bootRig) wire(r *zduRig) {
	r.o.linkPause = func() int { b.mu.Lock(); b.pause++; b.mu.Unlock(); return 1 }
	r.o.linkResume = func() int { b.mu.Lock(); b.resume++; b.mu.Unlock(); return 1 }
	r.o.boost = func(on bool) (int, int) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if on {
			b.on++
			return 10, 0
		}
		b.offCnt++
		return 0, 0
	}
}

func (b *bootRig) get() (int, int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pause, b.resume, b.on, b.offCnt
}

// Review issue 4: the old process moves link accept to the new process and
// boosts the poolctl target before the new process needs links, and clears
// the boost at the commit without re-opening accept. Review M2: with a child
// without LISTENING (no FeatListening) that is right after FDS, before
// READY; with LISTENING see TestZDUTakeoverBootAfterListening.
func TestZDUTakeoverBootsLinks(t *testing.T) {
	r := newRig(t, okTime)
	var b bootRig
	b.wire(r)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		tk, err := c.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
		if err != nil {
			ch.Kill()
			return
		}
		// the links must be the new process's before its READY (warm wait)
		for i := 0; i < 100; i++ {
			if p, _, on, _ := b.get(); p == 1 && on == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if p, _, on, _ := b.get(); p != 1 || on != 1 {
			t.Errorf("before READY: link pause %d boost %d", p, on)
		}
		if tk.Ready(time.Second) != nil {
			ch.Kill()
			return
		}
		go tk.Conn.FollowDrain(func(int) {}, 5*time.Second)
	}
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if p, res, on, off := b.get(); p != 1 || res != 0 || on != 1 || off != 1 {
		t.Fatalf("pause %d resume %d boost on %d off %d", p, res, on, off)
	}
	if !strings.Contains(r.logText(), "link accept moved to it") {
		t.Fatal(r.logText())
	}
}

// A failed handover gives the links back: accept again here, boost cleared.
func TestZDUTakeoverBootUndoneOnFail(t *testing.T) {
	r := newRig(t, okTime)
	var b bootRig
	b.wire(r)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		c.Take(handoff.Hello{Proto: handoff.Proto}, time.Second)
		ch.Kill() // no READY (e.g. not warm): the old keeps serving
	}
	if got := r.o.handover(); got != "failed" {
		t.Fatalf("result %q", got)
	}
	if p, res, on, off := b.get(); p != 1 || res != 1 || on != 1 || off != 1 {
		t.Fatalf("pause %d resume %d boost on %d off %d", p, res, on, off)
	}
}

// A revert in probation gives the links back too.
func TestZDUTakeoverBootUndoneOnRevert(t *testing.T) {
	r := newRig(t, okTime)
	var b bootRig
	b.wire(r)
	r.behave = func(c *handoff.Conn, ch *fakeChild) {
		goodChild(c, ch)
		time.Sleep(100 * time.Millisecond)
		ch.Kill() // dies in probation
	}
	if got := r.o.handover(); got != "reverted" {
		t.Fatalf("result %q\n%s", got, r.logText())
	}
	if _, res, _, off := b.get(); res != 1 || off != 1 {
		t.Fatalf("resume %d boost off %d", res, off)
	}
}

// Review issue 6: users 0 is not the drain end while TUN packets still come
// from the links; the end waits for the L3 quiet.
func TestZDUDrainWaitsL3Quiet(t *testing.T) {
	r := newRig(t, okTime)
	r.behave = goodChild
	var mu sync.Mutex
	quiet := false
	r.o.l3Quiet = func(time.Time) (bool, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return quiet, 100 * time.Millisecond
	}
	r.statsU = 0
	if got := r.o.handover(); got != "ok" {
		t.Fatalf("result %q", got)
	}
	select {
	case <-r.exited:
		t.Fatal("exited at users 0 with TUN packets still arriving")
	case <-time.After(300 * time.Millisecond):
	}
	if !strings.Contains(r.logText(), "a TUN packet came from a link") {
		t.Fatal(r.logText())
	}
	mu.Lock()
	quiet = true
	mu.Unlock()
	select {
	case <-r.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit after the L3 quiet")
	}
}

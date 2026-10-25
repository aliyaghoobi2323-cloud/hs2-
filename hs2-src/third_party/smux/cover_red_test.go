package smux

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// The H2 red team (the cover): the covers' accounting in the pool across
// links whose window core takes and gives back their covers at every update
// (Session.SetCover flipping from another goroutine, as the runner's apply
// does), ceilings that come and go, and streams and sessions that close
// while covered. While they run the covers hold at most their share of the
// pool and the count of covered streams matches the sessions'; once every
// session closed the pool is whole again, with no cover's bytes and no
// covered stream left counted.
func TestCoverRedAccountingUnderFlaps(t *testing.T) {
	const size = 2 << 20
	pool := NewWindowPool(size)
	cfg := poolConfig(pool)
	type link struct {
		s  *Session
		st *Stream
		p  *rawPeer
	}
	var links []link
	for i := 0; i < 6; i++ {
		s, st, p := newRawPeer(t, cfg)
		links = append(links, link{s, st, p})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rnd := rand.New(rand.NewPCG(1, 2))
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, l := range links {
				l.s.SetCover(rnd.IntN(3) != 0)
				if rnd.IntN(8) == 0 {
					l.st.SetCoreCeil(uint32(rnd.IntN(2)) * uint32(cfg.MaxStreamBuffer))
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	check := func(when string) {
		t.Helper()
		if c := pool.Covers(); int64(c) > pool.size/coverShare || c < 0 {
			t.Fatalf("%s: covers hold %d of a pool of %d", when, c, size)
		}
		n := 0
		for _, l := range links {
			n += l.s.CoverStats().Now
		}
		if n != pool.CoverStreams() || n < 0 {
			t.Fatalf("%s: sessions count %d covered streams, the pool %d", when, n, pool.CoverStreams())
		}
		if f := pool.Free(); f < 0 || f > size {
			t.Fatalf("%s: pool free %d of %d", when, f, size)
		}
	}
	for round := 0; round < 6; round++ {
		for _, l := range links {
			heldRounds(t, l.p, l.st, 3, 2*time.Millisecond)
		}
		check("running")
	}
	close(stop)
	wg.Wait()
	// half the links lose their stream while covered, then every session
	// closes
	for i, l := range links {
		if i%2 == 0 {
			l.st.Close()
		}
	}
	for _, l := range links {
		l.s.Close()
	}
	for _, l := range links {
		l.st.Close() // (its app lets go of it: recycleTokens)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pool.Free() != size || pool.Covers() != 0 || pool.CoverStreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("all closed: pool free %d of %d, covers hold %d, %d covered streams", pool.Free(), size, pool.Covers(), pool.CoverStreams())
		}
		time.Sleep(time.Millisecond)
	}
	var g uint32
	for _, l := range links {
		c := l.s.CoverStats()
		g += c.Grants
		if c.Lost() > c.Grants+c.Back {
			t.Errorf("a session lost %d covers of %d granted and %d given back: %+v", c.Lost(), c.Grants, c.Back, c)
		}
	}
	if g == 0 {
		t.Fatal("no cover granted: the test tested nothing")
	}
}

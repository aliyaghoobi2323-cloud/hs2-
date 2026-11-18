package engine

import (
	"sync"
	"testing"
	"time"
)

// b307: the sharded conn set counts exactly what the single map did, under
// concurrent add/done and a sampler running beside them.
func TestB307ExitShardsCount(t *testing.T) {
	tr := newExitTraffic(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	sdone := make(chan struct{})
	go func() { // a sampler racing add/done
		defer close(sdone)
		now := time.Now()
		for {
			select {
			case <-stop:
				return
			default:
				now = now.Add(healthTick)
				tr.sample(now)
			}
		}
	}()
	keep := make([][]*exitConn, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				c := tr.open(nil)
				if i%3 == 0 {
					keep[g] = append(keep[g], c)
				} else {
					tr.done(c)
				}
				tr.done(&exitConn{}) // never added: a no-op
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	<-sdone
	want := 0
	for _, k := range keep {
		want += len(k)
	}
	tr.sample(time.Now().Add(time.Hour))
	if s := tr.snap.Load(); s == nil || s.open != want {
		t.Fatalf("open %v, want %d", s, want)
	}
	used := 0
	for i := range tr.sh {
		if len(tr.sh[i].conns) > 0 {
			used++
		}
	}
	if used < exitShards/2 {
		t.Fatalf("only %d of %d shards used", used, exitShards)
	}
	for _, k := range keep {
		for _, c := range k {
			tr.done(c)
			tr.done(c) // double done: a no-op
		}
	}
	tr.sample(time.Now().Add(2 * time.Hour))
	if s := tr.snap.Load(); s.open != 0 {
		t.Fatalf("open %d after all done", s.open)
	}
}

// a sub's link meter is counted once per add and uncounted once per done
func TestB307ExitShardsMeterUsers(t *testing.T) {
	tr := newExitTraffic(nil)
	mtr := &linkMeter{}
	cs := make([]*exitConn, 500)
	for i := range cs {
		cs[i] = tr.openSub(mtr)
	}
	if n := mtr.users.Load(); n != 500 {
		t.Fatalf("users %d", n)
	}
	for _, c := range cs {
		tr.done(c)
		tr.done(c)
	}
	if n := mtr.users.Load(); n != 0 {
		t.Fatalf("users %d after done", n)
	}
}

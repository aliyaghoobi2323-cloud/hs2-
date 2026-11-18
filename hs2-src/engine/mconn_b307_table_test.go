package engine

import (
	"sync"
	"testing"
	"time"
)

// The sharded table keeps the single-lock table semantics per id and its
// counts: create/join/remove from many goroutines on many ids, then size.
func TestB307MConnTableShards(t *testing.T) {
	tbl := newMConnTable()
	const n = 4000
	ms := make([]*mConn, n)
	for i := 0; i < n; i++ {
		ms[i] = newMConn(uint64(i), false, nil)
	}
	stop := make(chan struct{})
	got := make([]*mConn, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); got[i] = tbl.join(uint64(i), 5*time.Second, stop) }(i)
		go func(i int) {
			defer wg.Done()
			if !tbl.create(uint64(i), ms[i]) {
				t.Errorf("create %d", i)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if got[i] != ms[i] {
			t.Fatalf("join %d got %p want %p", i, got[i], ms[i])
		}
		if tbl.create(uint64(i), newMConn(uint64(i), false, nil)) {
			t.Fatalf("id %d taken twice", i)
		}
	}
	if tbl.size() != n {
		t.Fatalf("size %d", tbl.size())
	}
	used := 0
	for i := range tbl.sh {
		if len(tbl.sh[i].m) > 0 {
			used++
		}
	}
	if used < mconnShards*3/4 {
		t.Fatalf("only %d of %d shards used", used, mconnShards)
	}
	for i := 0; i < n; i += 2 {
		tbl.remove(uint64(i), newMConn(0, false, nil)) // not its mc: kept
		tbl.remove(uint64(i), ms[i])
	}
	if tbl.size() != n/2 {
		t.Fatalf("size after remove %d", tbl.size())
	}
	if tbl.peek(0) != nil || tbl.peek(1) != ms[1] {
		t.Fatal("peek")
	}
	for i := 1; i < n; i += 2 {
		tbl.remove(uint64(i), ms[i])
	}
	if tbl.size() != 0 {
		t.Fatalf("size at the end %d", tbl.size())
	}
}

// A join that times out leaves no slot; a reserved (unpublished) conn
// removed wakes its joins with nil.
func TestB307MConnTableJoinEdges(t *testing.T) {
	tbl := newMConnTable()
	if tbl.join(7, 10*time.Millisecond, nil) != nil || tbl.size() != 0 || tbl.has(7) {
		t.Fatal("timed-out join left a slot")
	}
	mc := newMConn(9, false, nil)
	if !tbl.reserve(9, mc) {
		t.Fatal("reserve")
	}
	r := make(chan *mConn)
	go func() { r <- tbl.join(9, time.Millisecond, nil) }()
	time.Sleep(20 * time.Millisecond)
	tbl.remove(9, mc)
	if got := <-r; got != nil {
		t.Fatal("join of a removed unpublished conn got it")
	}
	if tbl.size() != 0 {
		t.Fatalf("size %d", tbl.size())
	}
}

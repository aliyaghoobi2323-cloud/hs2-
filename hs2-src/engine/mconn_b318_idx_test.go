package engine

import (
	"sync"
	"testing"
	"unsafe"
)

// b318: the sharded ul268 index under a reconnect storm: many goroutines add
// and remove subs on shared link keys (pointer and string keys) while a
// visitor reads; at the end the index holds exactly the subs still added.
func TestB318IdxStormConcurrent(t *testing.T) {
	if sz := unsafe.Sizeof(ul268IdxShard{}); sz != 64 {
		t.Fatalf("shard size %d, want 64", sz)
	}
	type lk struct{ n int }
	keys := []any{"S1", "S2", &lk{1}, &lk{2}, &lk{3}, 7}
	const G, N = 32, 300
	var wg sync.WaitGroup
	kept := make([][]*msub, G)
	stop := make(chan struct{})
	var vw sync.WaitGroup
	vw.Add(1)
	go func() { // rescueWatch-like visitor
		defer vw.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ul268ConnsOn(keys)
			ul268IdxLen()
		}
	}()
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			m := &mConn{iran: true}
			for i := 0; i < N; i++ {
				s := &msub{m: m, key: keys[(g+i)%len(keys)]}
				ul268IdxAdd(s)
				if i%3 == 0 {
					kept[g] = append(kept[g], s)
					continue
				}
				ul268IdxDel(s)
				ul268IdxDel(s) // close after removeSubLocked
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	vw.Wait()
	want := 0
	for g := range kept {
		for _, s := range kept[g] {
			if in, _ := ul268IdxHas(s.key, s); !in {
				t.Fatalf("kept sub missing on %v", s.key)
			}
			want++
		}
	}
	got, by := ul268ConnsOn(keys)
	if len(got) != G {
		t.Fatalf("conns %d want %d", len(got), G)
	}
	n := 0
	for _, ks := range by {
		n += len(ks)
	}
	if n != want {
		t.Fatalf("indexed subs %d want %d", n, want)
	}
	for g := range kept {
		for _, s := range kept[g] {
			ul268IdxDel(s)
		}
	}
	for _, k := range keys {
		if _, entry := ul268IdxHas(k, nil); entry {
			t.Fatalf("empty entry kept for %v", k)
		}
	}
	if l, s := ul268IdxLen(); l != 0 || s != 0 {
		t.Fatalf("index not empty: links %d subs %d", l, s)
	}
}

// b318: one key always maps to one shard (interface keys hash by value).
func TestB318IdxShardStable(t *testing.T) {
	type lk struct{ n int }
	p := &lk{1}
	if ul268IdxShardOf(any(p)) != ul268IdxShardOf(any(p)) || ul268IdxShardOf("x") != ul268IdxShardOf("x") {
		t.Fatal("shard not stable")
	}
	seen := map[*ul268IdxShard]bool{}
	for i := 0; i < 1000; i++ {
		seen[ul268IdxShardOf(any(&lk{i}))] = true
	}
	if len(seen) < ul268Shards/2 {
		t.Fatalf("keys spread over only %d shards", len(seen))
	}
}

func BenchmarkB318IdxAddDelParallel(b *testing.B) {
	type lk struct{ n int }
	keys := make([]any, 200)
	for i := range keys {
		keys[i] = &lk{i}
	}
	b.RunParallel(func(pb *testing.PB) {
		m := &mConn{iran: true}
		i := 0
		for pb.Next() {
			s := &msub{m: m, key: keys[i%len(keys)]}
			ul268IdxAdd(s)
			ul268IdxDel(s)
			i++
		}
	})
}

// the pre-b318 single-mutex index, for the benchmark comparison only.
var b318OldIdx struct {
	mu sync.Mutex
	m  map[any]map[*msub]struct{}
}

func BenchmarkB318IdxOldGlobalParallel(b *testing.B) {
	type lk struct{ n int }
	keys := make([]any, 200)
	for i := range keys {
		keys[i] = &lk{i}
	}
	b318OldIdx.m = map[any]map[*msub]struct{}{}
	b.RunParallel(func(pb *testing.PB) {
		m := &mConn{iran: true}
		i := 0
		for pb.Next() {
			s := &msub{m: m, key: keys[i%len(keys)]}
			b318OldIdx.mu.Lock()
			e := b318OldIdx.m[s.key]
			if e == nil {
				e = map[*msub]struct{}{}
				b318OldIdx.m[s.key] = e
			}
			e[s] = struct{}{}
			b318OldIdx.mu.Unlock()
			b318OldIdx.mu.Lock()
			delete(b318OldIdx.m[s.key], s)
			if len(b318OldIdx.m[s.key]) == 0 {
				delete(b318OldIdx.m, s.key)
			}
			b318OldIdx.mu.Unlock()
			i++
		}
	})
}

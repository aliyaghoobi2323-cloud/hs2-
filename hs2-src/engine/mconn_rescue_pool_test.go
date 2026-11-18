package engine

import (
	"bytes"
	"math/rand"
	"sync"
	"testing"
)

func TestGC296RingPoolClass(t *testing.T) {
	for n, want := range map[int]int{4 << 10: 0, 8 << 10: 1, 128 << 10: 5, 8 << 20: 11, 0: -1, 4095: -1, 6 << 10: -1, 16 << 20: -1, 36 << 10: -1} {
		if got := rpClass(n); got != want {
			t.Errorf("rpClass(%d) = %d, want %d", n, got, want)
		}
	}
	b := rpGet(64 << 10)
	if len(b) != 64<<10 || cap(b) != 64<<10 {
		t.Fatalf("rpGet len %d cap %d", len(b), cap(b))
	}
	if b := rpGet(36 << 10); len(b) != 36<<10 {
		t.Fatalf("rpGet off-class len %d", len(b))
	}
}

// rpPoison fills every class with garbage buffers so that a ring relying on
// a zeroed buffer (or reading past its n bytes) shows.
func rpPoison() {
	for c := range rpPools {
		if c > 6 { // up to 256 KB is enough here
			break
		}
		for i := 0; i < 4; i++ {
			b := make([]byte, 1<<(c+rpMinShift))
			for j := range b {
				b[j] = 0xA5
			}
			rpPut(b)
		}
	}
}

// gc296RingModel: random adds/gets/releases on one ring against the plain
// byte stream it was fed.
func gc296RingModel(t *testing.T, rng *rand.Rand, ops int) {
	r := &rescueRing{on: true}
	defer r.release()
	var all []byte // every byte since the last release
	var end uint64
	for i := 0; i < ops; i++ {
		switch op := rng.Intn(100); {
		case op < 70:
			n := 1 + rng.Intn(9000)
			if rng.Intn(20) == 0 {
				n = 1 + rng.Intn(200<<10)
			}
			p := make([]byte, n)
			rng.Read(p)
			r.add(p)
			if !r.on {
				t.Errorf("ring turned off (budget)")
				return
			}
			all = append(all, p...)
			end += uint64(n)
			if len(all) > 1<<20 {
				all = append([]byte(nil), all[len(all)-(256<<10):]...)
			}
		case op < 95:
			if r.n > len(all) {
				t.Errorf("ring holds %d > fed %d", r.n, len(all))
				return
			}
			back := uint64(rng.Intn(r.n + 1))
			got, ok := r.get(end-back, end)
			if !ok {
				t.Errorf("get back %d of n %d: not covered", back, r.n)
				return
			}
			if !bytes.Equal(got, all[len(all)-int(back):]) {
				t.Errorf("op %d: ring content differs (back %d n %d cap %d)", i, back, r.n, len(r.buf))
				return
			}
		default:
			r.release()
			all = all[:0]
		}
	}
}

func TestGC296RingPoolModel(t *testing.T) {
	rpPoison()
	rng := rand.New(rand.NewSource(296))
	for k := 0; k < 20; k++ {
		gc296RingModel(t, rng, 400)
	}
	if rpStat.got.Load() == 0 || rpStat.put.Load() == 0 {
		t.Fatalf("pool not used: got %d put %d", rpStat.got.Load(), rpStat.put.Load())
	}
}

// TestGC296RingPoolConcurrent: many rings (one lock each, as wmu/rkMu) share
// the pools; a buffer handed to two rings at once shows as wrong content or,
// under -race, as a race.
func TestGC296RingPoolConcurrent(t *testing.T) {
	rpPoison()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for k := 0; k < 8; k++ {
				gc296RingModel(t, rng, 150)
			}
		}(int64(g))
	}
	wg.Wait()
}

func TestGC296RingPoolOff(t *testing.T) {
	rpOn.Store(false)
	defer rpOn.Store(true)
	g0, p0 := rpStat.got.Load(), rpStat.put.Load()
	gc296RingModel(t, rand.New(rand.NewSource(7)), 300)
	if rpStat.got.Load() != g0 || rpStat.put.Load() != p0 {
		t.Fatalf("pool used while off")
	}
}

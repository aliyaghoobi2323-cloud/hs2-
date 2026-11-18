package engine

import (
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"
)

// gc296: rescue ring buffers come from and go back to per-size-class pools.
//
// gc295 pprof: rescueRing.add growth (4 KB -> x2 -> cap, one make per step,
// the old buffer dropped) was the largest exit allocator left (kh 613 MB/min
// in the storm). A ring buffer never leaves its ring: every access (add,
// get, copyOut, shrinkIdle, release) runs under the ring's one lock (the
// raw ring under m.wmu, the records ring under m.rkMu), get/copyOut copy
// into a fresh slice, and only len(buf) is read elsewhere. So a buffer the
// ring replaced (growth, shrink) or freed (release) has no other reader and
// can be reused at once by another ring.
//
// Classes: power-of-two sizes from 4 KB to 8 MB (ul268ConnMax default).
// Other sizes (a rate-scaled cap, a shrink to 4 KB steps) are made and
// dropped as before. A pooled buffer is not zeroed: the ring only reads the
// n bytes it wrote. HS2_GC296_RINGPOOL=0 turns it off (make/drop as before).

const (
	rpMinShift = 12 // 4 KB
	rpMaxShift = 23 // 8 MB
)

var (
	rpOn    atomic.Bool
	rpPools [rpMaxShift - rpMinShift + 1]sync.Pool
	rpStat  struct {
		got, made, put atomic.Uint64
	}
)

func init() { rpOn.Store(envIntDef("HS2_GC296_RINGPOOL", 1) != 0) }

// rpClass: the pool index for a buffer of exactly n bytes, -1 if none.
func rpClass(n int) int {
	if n < 1<<rpMinShift || n > 1<<rpMaxShift || n&(n-1) != 0 {
		return -1
	}
	return bits.TrailingZeros(uint(n)) - rpMinShift
}

// rpGet: a buffer of exactly n bytes (contents undefined when pooled).
func rpGet(n int) []byte {
	if c := rpClass(n); c >= 0 && rpOn.Load() {
		if p, _ := rpPools[c].Get().(*[]byte); p != nil {
			rpStat.got.Add(1)
			return *p
		}
		rpStat.made.Add(1)
	}
	return make([]byte, n)
}

// rpPut: b is no longer referenced by its ring.
func rpPut(b []byte) {
	if c := rpClass(cap(b)); c >= 0 && len(b) == cap(b) && rpOn.Load() {
		rpStat.put.Add(1)
		rpPools[c].Put(&b)
	}
}

// rpLogLine: the pool counters for the rescue stats line.
func rpLogLine() string {
	return fmt.Sprintf("gc296 ringpool on=%v got %d made %d put %d", rpOn.Load(), rpStat.got.Load(), rpStat.made.Load(), rpStat.put.Load())
}

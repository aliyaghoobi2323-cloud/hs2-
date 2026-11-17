package smux

import "sync/atomic"

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

type shaperHeap []writeRequest

func (h shaperHeap) Len() int { return len(h) }
func (h shaperHeap) Less(i, j int) bool {
	if h[i].class != h[j].class {
		return h[i].class < h[j].class
	}
	return _itimediff(h[j].seq-h[j].jump, h[i].seq-h[i].jump) > 0
}

func (h shaperHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *shaperHeap) Push(x interface{}) { *h = append(*h, x.(writeRequest)) }

func (h *shaperHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// SmallFirstJump (hs2, Config.SmallFirst): how many earlier queued requests a
// small data frame may overtake. The shaper holds at most maxShaperSize
// requests and every writing stream has one frame in it at a time, so 256
// puts a small frame ahead of every bulk frame of a busy link, while a bulk
// frame still goes once 256 later requests have been queued — never starved.
const SmallFirstJump = 256

// smallFirstOvertakes counts the small frames (Config.SmallFirst) written
// ahead of an earlier queued data frame, process-wide: the proof the
// scheduler acts (HS2_SMUX_SMALL_FIRST, obs_smux_small_first).
var smallFirstOvertakes atomic.Uint64

// SmallFirstOvertakes returns smallFirstOvertakes.
func SmallFirstOvertakes() uint64 { return smallFirstOvertakes.Load() }

// overtook: a data frame queued before w is still waiting in h.
func overtook(h shaperHeap, w writeRequest) bool {
	for i := range h {
		if h[i].class == CLSDATA && _itimediff(w.seq, h[i].seq) > 0 {
			return true
		}
	}
	return false
}

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

// hs2 gc295: pushReq/popReq are container/heap's Push/Pop on the concrete
// type (the same up/down steps, so the same order): heap.Push/Pop box every
// writeRequest into an interface{} (two heap objects per frame through the
// shaper, ~5 M objects/min on a busy exit in the gc295 storm profile).
func (h *shaperHeap) pushReq(x writeRequest) {
	*h = append(*h, x)
	h.up(len(*h) - 1)
}

func (h *shaperHeap) popReq() writeRequest {
	old := *h
	n := len(old) - 1
	old.Swap(0, n)
	old.down(0, n)
	x := old[n]
	old[n] = writeRequest{} // no stale result/own pointers left in the backing array
	*h = old[:n]
	return x
}

func (h shaperHeap) up(j int) {
	for {
		i := (j - 1) / 2 // parent
		if i == j || !h.Less(j, i) {
			break
		}
		h.Swap(i, j)
		j = i
	}
}

func (h shaperHeap) down(i0, n int) {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 { // j1 < 0 after int overflow
			break
		}
		j := j1 // left child
		if j2 := j1 + 1; j2 < n && h.Less(j2, j1) {
			j = j2 // right child
		}
		if !h.Less(j, i) {
			break
		}
		h.Swap(i, j)
		i = j
	}
}

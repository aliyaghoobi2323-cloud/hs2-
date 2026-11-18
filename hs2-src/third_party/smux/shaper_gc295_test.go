package smux

import (
	"container/heap"
	"math/rand"
	"testing"
)

// gc295: pushReq/popReq give exactly container/heap's order (same up/down),
// including ties of seq-jump between a SmallFirst frame and a plain one, over
// random interleavings of pushes and pops.
func TestShaperTypedHeapSameOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(295))
	for round := 0; round < 200; round++ {
		var a, b shaperHeap
		seq := uint32(rng.Intn(1 << 20))
		for op := 0; op < 2000; op++ {
			if len(a) == 0 || rng.Intn(3) > 0 {
				seq++
				w := writeRequest{class: CLASSID(rng.Intn(2)), seq: seq, frame: Frame{sid: uint32(op)}}
				if rng.Intn(4) == 0 {
					w.jump = SmallFirstJump
				}
				heap.Push(&a, w)
				b.pushReq(w)
			} else {
				x := heap.Pop(&a).(writeRequest)
				y := b.popReq()
				if x.seq != y.seq || x.frame.sid != y.frame.sid {
					t.Fatalf("round %d op %d: container/heap popped seq %d sid %d, typed %d sid %d", round, op, x.seq, x.frame.sid, y.seq, y.frame.sid)
				}
			}
			if len(a) != len(b) {
				t.Fatalf("len %d vs %d", len(a), len(b))
			}
		}
	}
}

// gc295: a frame's result channel and owner state come from reqCtxPool and
// go back once the writer has its result; reuse must not leak a state.
func TestReqCtxPoolReset(t *testing.T) {
	rc := reqCtxPool.Get().(*reqCtx)
	if rc.own.st.Load() != reqPending || rc.own.data != nil || cap(rc.result) != 1 || len(rc.result) != 0 {
		t.Fatalf("fresh ctx not pending/empty")
	}
}

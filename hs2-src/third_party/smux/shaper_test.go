package smux

import (
	"container/heap"
	"testing"
)

func TestShaper(t *testing.T) {
	w1 := writeRequest{seq: 1}
	w2 := writeRequest{seq: 2}
	w3 := writeRequest{seq: 3}
	w4 := writeRequest{seq: 4}
	w5 := writeRequest{seq: 5}

	var reqs shaperHeap
	heap.Push(&reqs, w5)
	heap.Push(&reqs, w4)
	heap.Push(&reqs, w3)
	heap.Push(&reqs, w2)
	heap.Push(&reqs, w1)

	for len(reqs) > 0 {
		w := heap.Pop(&reqs).(writeRequest)
		t.Log("sid:", w.frame.sid, "seq:", w.seq)
	}
}

func TestShaper2(t *testing.T) {
	w1 := writeRequest{class: CLSDATA, seq: 1} // stream 0
	w2 := writeRequest{class: CLSDATA, seq: 2}
	w3 := writeRequest{class: CLSDATA, seq: 3}
	w4 := writeRequest{class: CLSDATA, seq: 4}
	w5 := writeRequest{class: CLSDATA, seq: 5}
	w6 := writeRequest{class: CLSCTRL, seq: 6, frame: Frame{sid: 10}} // ctrl 1
	w7 := writeRequest{class: CLSCTRL, seq: 7, frame: Frame{sid: 11}} // ctrl 2

	var reqs shaperHeap
	heap.Push(&reqs, w6)
	heap.Push(&reqs, w5)
	heap.Push(&reqs, w4)
	heap.Push(&reqs, w3)
	heap.Push(&reqs, w2)
	heap.Push(&reqs, w1)
	heap.Push(&reqs, w7)

	for len(reqs) > 0 {
		w := heap.Pop(&reqs).(writeRequest)
		t.Log("sid:", w.frame.sid, "seq:", w.seq)
	}
}

// hs2 SmallFirst: a small frame overtakes earlier bulk frames, by at most
// SmallFirstJump requests.
func TestShaperSmallFirst(t *testing.T) {
	var reqs shaperHeap
	for i := uint32(1); i <= 5; i++ {
		heap.Push(&reqs, writeRequest{class: CLSDATA, seq: i, frame: Frame{sid: 1}})
	}
	heap.Push(&reqs, writeRequest{class: CLSDATA, seq: 6, jump: SmallFirstJump, frame: Frame{sid: 2}})
	heap.Push(&reqs, writeRequest{class: CLSCTRL, seq: 7, frame: Frame{sid: 3}})
	if w := heap.Pop(&reqs).(writeRequest); w.frame.sid != 3 {
		t.Fatalf("first = sid %d, want the control frame", w.frame.sid)
	}
	if w := heap.Pop(&reqs).(writeRequest); w.frame.sid != 2 {
		t.Fatalf("second = sid %d, want the small frame", w.frame.sid)
	}
	if !overtook(reqs, writeRequest{class: CLSDATA, seq: 6}) || overtook(reqs, writeRequest{class: CLSDATA, seq: 0}) {
		t.Fatal("overtook: wrong")
	}
	// bounded: a bulk frame queued more than SmallFirstJump requests earlier goes first
	reqs = nil
	heap.Push(&reqs, writeRequest{class: CLSDATA, seq: 10 + SmallFirstJump + 1, jump: SmallFirstJump, frame: Frame{sid: 2}})
	heap.Push(&reqs, writeRequest{class: CLSDATA, seq: 10, frame: Frame{sid: 1}})
	if w := heap.Pop(&reqs).(writeRequest); w.frame.sid != 1 {
		t.Fatalf("starved: sid %d first", w.frame.sid)
	}
}

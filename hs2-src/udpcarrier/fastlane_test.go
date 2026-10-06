package udpcarrier

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// An urgent data shard (an interactive flow's packet: SendUrgent) leaves
// ahead of the data the pacer already holds, and is never held back by the
// data queue's time bound — a ping does not wait behind a download.
func TestPacerFastLane(t *testing.T) {
	var mu sync.Mutex
	var order []uint16
	write := func(b []byte) error {
		mu.Lock()
		order = append(order, binary.BigEndian.Uint16(b[len(b)-2:]))
		mu.Unlock()
		return nil
	}
	rc := newRateControl() // ~1 Mbit/s: a 1200-byte shard every ~10 ms
	p := newPacer(rc, write, 64, nil)
	defer p.close()
	shard := func(i uint16) []byte {
		b := make([]byte, 1200) // byte 5 zero: a data shard, not parity
		binary.BigEndian.PutUint16(b[len(b)-2:], i)
		return b
	}
	go func() {
		for i := uint16(1); i <= 40; i++ {
			p.enqueue(shard(i)) // blocks while ~20 ms of data waits
		}
	}()
	sent := func() int { mu.Lock(); defer mu.Unlock(); return len(order) }
	deadline := time.Now().Add(3 * time.Second)
	for sent() < 8 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	at := sent()
	start := time.Now()
	p.enqueueLane(shard(999), true) // must not block behind the data queue
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Fatalf("urgent enqueue blocked %v behind the data queue", d)
	}
	for time.Now().Before(deadline) {
		mu.Lock()
		for i, v := range order {
			if v == 999 {
				mu.Unlock()
				if i > at+1 {
					t.Fatalf("urgent shard sent %d datagrams after it was queued (want at most 1: the one already leaving)", i-at)
				}
				return
			}
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("urgent shard never sent")
}

// The data lane's counters: LaneDrained(mark) turns true only once every
// shard queued up to the mark has been sent (the pool's ordering guard).
func TestPacerLaneCounters(t *testing.T) {
	var mu sync.Mutex
	n := 0
	write := func(b []byte) error { mu.Lock(); n++; mu.Unlock(); return nil }
	rc := newRateControl()
	p := newPacer(rc, write, 64, nil)
	defer p.close()
	c := &Conn{pacer: p}
	for i := 0; i < 5; i++ {
		p.enqueue(make([]byte, 1200))
	}
	mark := c.LaneMark()
	if mark != 5 {
		t.Fatalf("mark %d after 5 data shards, want 5", mark)
	}
	p.enqueueLane(make([]byte, 100), true) // the fast lane is not counted
	if c.LaneMark() != 5 {
		t.Fatal("a fast-lane shard moved the mark")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !c.LaneDrained(mark) {
		mu.Lock()
		sent := n
		mu.Unlock()
		if sent >= 6 && !c.LaneDrained(mark) {
			t.Fatalf("all %d sent but not drained", sent)
		}
		if time.Now().After(deadline) {
			t.Fatal("never drained")
		}
		time.Sleep(time.Millisecond)
	}
}

// With a batch sender the pacer hands several waiting datagrams to one call:
// every datagram exactly once, in wire order (the sequence it stamps), parity
// still ahead of queued data, and no faster than the paced rate.
func TestPacerBatches(t *testing.T) {
	var mu sync.Mutex
	var seqs []uint32
	batches, multi, biggest := 0, 0, 0
	record := func(b []byte) {
		seqs = append(seqs, binary.BigEndian.Uint32(b[1:5]))
	}
	rc := newRateControl()
	rc.rate = 2e6 // 16 Mbit/s
	p := newPacer(rc, func(b []byte) error { mu.Lock(); record(b); batches++; mu.Unlock(); return nil }, 256, nil)
	defer p.close()
	wb := func(bs [][]byte) error {
		mu.Lock()
		defer mu.Unlock()
		batches++
		if len(bs) > 1 {
			multi++
		}
		biggest = max(biggest, len(bs))
		for _, b := range bs {
			record(b)
		}
		return nil
	}
	p.writeBatch.Store(&wb)
	const n = 300
	start := time.Now()
	go func() {
		for i := 0; i < n; i++ {
			p.enqueue(make([]byte, 1200))
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(seqs)
		mu.Unlock()
		if got == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d sent", got, n)
		}
		time.Sleep(time.Millisecond)
	}
	el := time.Since(start)
	mu.Lock()
	defer mu.Unlock()
	for i := range seqs {
		if seqs[i] != uint32(i) {
			t.Fatalf("datagram %d carries wire sequence %d", i, seqs[i])
		}
	}
	if multi == 0 {
		t.Fatalf("no batch of more than one in %d sends", batches)
	}
	// A batch carries no more than the bucket holds (2 ms of the rate: ~4 KB,
	// three datagrams and the one being paid for) — not a burst of 16.
	if biggest > 5 {
		t.Fatalf("a batch of %d datagrams: more than the bucket can pay for", biggest)
	}
	// 300 × ~1209 bytes at 2 MB/s is ~180 ms; allow the bucket's burst.
	if el < 120*time.Millisecond {
		t.Fatalf("sent in %v: faster than the paced rate", el)
	}
}

// The pacer's send-stage counters: a writer held back by the data queue's
// time bound shows as held time, every socket write is counted and timed,
// and the datagrams it carried add up to what was sent.
func TestPacerDiag(t *testing.T) {
	var mu sync.Mutex
	n := 0
	write := func(b []byte) error {
		time.Sleep(200 * time.Microsecond) // a slow socket: the fd lock, the syscall
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	}
	rc := newRateControl() // ~1 Mbit/s: 30 shards of 1200 bytes take ~300 ms
	p := newPacer(rc, write, 64, nil)
	defer p.close()
	for i := 0; i < 30; i++ {
		p.enqueue(make([]byte, 1200))
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.diag().Sent < 30 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d := p.diag()
	if d.Sent != 30 || d.Writes == 0 || d.Writes > d.Sent {
		t.Fatalf("sent %d in %d writes", d.Sent, d.Writes)
	}
	if d.Write < time.Duration(d.Writes)*150*time.Microsecond {
		t.Fatalf("%d writes of ≥200 µs timed as %v", d.Writes, d.Write)
	}
	if d.Held < 100*time.Millisecond {
		t.Fatalf("30 shards at ~1 Mbit/s past a ~20 ms queue bound held the writer only %v", d.Held)
	}
	if d.Queued != 0 {
		t.Fatalf("%d bytes still queued after everything was sent", d.Queued)
	}
}

// A pacer whose writes come back late (a busy server: the pacer goroutine put
// aside) loses everything past the 2 ms bucket cap each round, so it sends a
// fraction of its allowance. When the rate model says the send stage holds
// the carrier back (stageLimited), it keeps the credit of up to 10 ms of
// lateness and sends about its allowance; any other carrier keeps the 2 ms
// cap (TestPacerBatches).
func TestPacerLateCreditOnlyWhenStageLimited(t *testing.T) {
	run := func(stage bool) float64 {
		rc := newRateControl()
		rc.rate = 2e6 // 16 Mbit/s: ~1670 datagrams of 1200 bytes a second
		rc.stageOn.Store(stage)
		var mu sync.Mutex
		sent := 0
		p := newPacer(rc, func(b []byte) error { time.Sleep(6 * time.Millisecond); mu.Lock(); sent++; mu.Unlock(); return nil }, 512, nil)
		defer p.close()
		wb := func(bs [][]byte) error {
			time.Sleep(6 * time.Millisecond) // every call returns late
			mu.Lock()
			sent += len(bs)
			mu.Unlock()
			return nil
		}
		p.writeBatch.Store(&wb)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				p.enqueue(make([]byte, 1200)) // always a backlog
			}
		}()
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		s0 := sent
		mu.Unlock()
		const window = 600 * time.Millisecond
		time.Sleep(window)
		mu.Lock()
		s1 := sent
		mu.Unlock()
		close(stop)
		return float64(s1-s0) * 1200 / window.Seconds() / rc.rate
	}
	off, on := run(false), run(true)
	t.Logf("share of the allowance sent: %.2f with the 2 ms cap, %.2f with the late credit", off, on)
	if off > 0.6 {
		t.Fatalf("without the stage flag the pacer sent %.2f of its allowance: the 2 ms cap did not hold", off)
	}
	if on < 0.75 || on > 1.15 {
		t.Fatalf("with the stage flag the pacer sent %.2f of its allowance, want about all of it and no more", on)
	}
}

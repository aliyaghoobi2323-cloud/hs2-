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

package engine

import "sync"

// tunBatch gathers the packets one carrier delivers together (what its read
// loop found waiting, what its reorderer released) and writes them to the
// TUN in one WriteBatch: with the TUN's TCP offloads (tun.Options.Offload),
// consecutive segments of one connection go in as one packet — one write()
// for dozens of segments, where every segment used to cost one. A TUN without
// WriteBatch gets them one by one, as before.
type tunBatch struct {
	mu      sync.Mutex
	pkts    [][]byte
	dev     tunWriter
	bw      interface{ WriteBatch([][]byte) (int, error) }
	written func(n int)
}

func newTunBatch(dev tunWriter, written func(int)) *tunBatch {
	b := &tunBatch{dev: dev, written: written}
	b.bw, _ = dev.(interface{ WriteBatch([][]byte) (int, error) })
	return b
}

// add queues one packet for the next flush; without WriteBatch it is written
// at once. The slice must stay untouched until then.
func (b *tunBatch) add(pkt []byte) {
	if b.bw == nil {
		if _, err := b.dev.Write(pkt); err == nil {
			b.written(1)
		}
		return
	}
	b.mu.Lock()
	b.pkts = append(b.pkts, pkt)
	b.mu.Unlock()
}

// flush writes what was queued.
func (b *tunBatch) flush() {
	if b.bw == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pkts) == 0 {
		return
	}
	n, _ := b.bw.WriteBatch(b.pkts) // a refused packet is a lost one, as with Write
	b.written(n)
	clear(b.pkts)
	b.pkts = b.pkts[:0]
}

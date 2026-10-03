package tlscarrier

import (
	"encoding/binary"
	"sync"
	"time"
)

// replayMem rejects reused auth nonces within the acceptance window. Bounded in
// time and size so a flood cannot exhaust memory — and in work: entries are
// kept in arrival order, so expiring them (and, past replayCap, dropping the
// oldest) costs O(1) per auth instead of a scan of the whole map on every one.
// (Dropping a live entry past the cap only matters to an attacker who already
// holds the shared key and made replayCap fresh auths within replayTTL.)
const (
	replayTTL = 5 * time.Minute
	replayCap = 16384
)

type replayEnt struct {
	k uint64
	t time.Time
}

type replayMem struct {
	mu   sync.Mutex
	m    map[uint64]time.Time
	fifo []replayEnt // arrival order; fifo[head:] are live
	head int
}

func newReplayMem() *replayMem { return &replayMem{m: make(map[uint64]time.Time)} }

func (r *replayMem) add(nonce []byte) bool {
	k := binary.BigEndian.Uint64(nonce[:8]) ^ binary.BigEndian.Uint64(nonce[8:16])
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.head < len(r.fifo) {
		e := r.fifo[r.head]
		if now.Sub(e.t) <= replayTTL && len(r.m) < replayCap {
			break
		}
		if t, ok := r.m[e.k]; ok && t.Equal(e.t) { // not since re-added
			delete(r.m, e.k)
		}
		r.head++
	}
	if r.head > 1024 && r.head*2 > len(r.fifo) { // compact the consumed front
		r.fifo = append(r.fifo[:0], r.fifo[r.head:]...)
		r.head = 0
	}
	if t, ok := r.m[k]; ok && now.Sub(t) <= replayTTL {
		return false
	}
	r.m[k] = now
	r.fifo = append(r.fifo, replayEnt{k, now})
	return true
}

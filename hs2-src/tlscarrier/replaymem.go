package tlscarrier

import (
	"encoding/binary"
	"sync"
	"time"
)

// replayMem rejects reused auth nonces within the acceptance window. Bounded in
// time and size so a flood cannot exhaust memory.
const (
	replayTTL = 5 * time.Minute
	replayCap = 16384
)

type replayMem struct {
	mu sync.Mutex
	m  map[uint64]time.Time
}

func newReplayMem() *replayMem { return &replayMem{m: make(map[uint64]time.Time)} }

func (r *replayMem) add(nonce []byte) bool {
	k := binary.BigEndian.Uint64(nonce[:8]) ^ binary.BigEndian.Uint64(nonce[8:16])
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) > replayCap {
		for kk, t := range r.m {
			if now.Sub(t) > replayTTL {
				delete(r.m, kk)
			}
		}
	}
	if t, ok := r.m[k]; ok && now.Sub(t) <= replayTTL {
		return false
	}
	r.m[k] = now
	return true
}

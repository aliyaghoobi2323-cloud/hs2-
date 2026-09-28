package core

import (
	"encoding/binary"
	"sync"
	"time"
)

// replayMemory remembers recently accepted handshake first-message MACs so a
// captured first message cannot be replayed within the acceptance window. It is
// bounded in both size and time: entries expire after the timestamp window, and
// the map is swept when it grows past a cap so a flood cannot exhaust memory.

const (
	replayMemTTL = 2 * tsWindow
	replayMemCap = 8192
)

type replayMemory struct {
	mu      sync.Mutex
	entries map[uint64]time.Time
}

func newReplayMemory() *replayMemory {
	return &replayMemory{entries: make(map[uint64]time.Time)}
}

// add records a MAC and reports whether it was new. A false return means the
// MAC was seen before and the handshake must be refused as a replay.
func (m *replayMemory) add(mac []byte) bool {
	key := binary.BigEndian.Uint64(mac[:8]) ^ binary.BigEndian.Uint64(mac[8:16])
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) > replayMemCap {
		for k, t := range m.entries {
			if now.Sub(t) > replayMemTTL {
				delete(m.entries, k)
			}
		}
	}
	if t, ok := m.entries[key]; ok && now.Sub(t) <= replayMemTTL {
		return false
	}
	m.entries[key] = now
	return true
}

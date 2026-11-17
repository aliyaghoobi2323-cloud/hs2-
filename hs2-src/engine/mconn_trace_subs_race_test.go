package engine

import (
	"sync"
	"testing"
	"time"
)

// sg154 (lab cd152 a64, rel64 exit, HS2_MCONN_TRACE): the per-sub trace read
// a sub's rw (subTCP) and key after dropping m.mu, while releaseRaw1 swapped
// the released raw first sub's rw to deadSubRW{} and its key to nil under
// m.mu. A torn interface read (exitCountedStream's type, deadSubRW's data
// pointer &runtime.zerobase) faulted at 0x10000000109. Under -race this test
// reports the race on the old code; on any build it must not crash.
func TestMConnTraceSubsVsReleaseRaw1(t *testing.T) {
	old := mconnDeadlineFix.Load()
	mconnDeadlineFix.Store(true)
	defer mconnDeadlineFix.Store(old)
	for i := 0; i < 200; i++ {
		m := newMConn(uint64(500+i), false, nil)
		m.tr = &mtrace{t0: time.Now(), id: uint64(500 + i)}
		m.trRx = &mtraceRx{}
		c := &exitConn{lastActive: time.Now(), mtr: &linkMeter{}, sub: true}
		m.mu.Lock()
		s := m.addSubLocked(exitCountedStream{nil, c}, any(Link(&mtcpLink{})), false)
		m.raw1 = s
		s.dead = true // its loops are gone (loops == 0) and its stream closed
		m.mu.Unlock()
		s.rwClosed.Store(true)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			now := time.Now()
			for k := 0; k < 20; k++ {
				now = now.Add(2 * time.Second) // past the once-a-second gate
				m.traceSubsTick(now)
			}
		}()
		go func() {
			defer wg.Done()
			m.releaseRaw1()
		}()
		wg.Wait()

		m.mu.Lock()
		_, released := s.rw.(deadSubRW)
		m.mu.Unlock()
		if !released {
			t.Fatal("the dead raw first sub was not released")
		}
		m.traceSubsTick(time.Now().Add(time.Hour)) // after the release: no socket, no crash
	}
}

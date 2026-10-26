package engine

import (
	"testing"
	"time"
)

func TestDiagTun(t *testing.T) {
	m, l, _ := attachedTun(t)
	ls := m.ls
	st := m.open(13)
	for range 100 {
		ls.pushTun(l, tunPkt4(1000, 1000, false), 11, 1)
	}
	for fl := uint32(2); fl < 22; fl++ {
		for range 64 {
			ls.pushTun(l, tunPkt4(uint16(fl), 200, false), 11, fl)
		}
	}
	time.Sleep(2 * tunFlowSojourn)
	ls.mu.Lock()
	_, ok := ls.next(&m.tb)
	ls.mu.Unlock()
	t.Logf("stale next %v", ok)
	for range 40 {
		ls.pushTun(l, tunPkt4(3000, 1400, false), 12, 30)
	}
	time.Sleep(2 * classTick)
	ls.tick(ctrlNow(), abRate)
	for range 10 {
		ls.pushTun(l, tunPkt4(3000, 1400, false), 12, 30)
	}
	it := m.take()
	t.Logf("first: tf %v pkts %d light %v", it.tf != nil, it.pkts, it.light)
	go st.Write(make([]byte, 128<<10))
	m.waitN(2, 0, 0)
	ls.mu.Lock()
	st2 := ls.s.Stats()
	u13 := ls.s.users[13]
	ls.mu.Unlock()
	t.Logf("stats %+v bucket13 %v", st2, u13.bucket)
}

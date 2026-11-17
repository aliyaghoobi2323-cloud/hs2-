package engine

import (
	"net"
	"testing"
	"time"
)

// fx247 (audit246 M3): the cs228 lite-visit prefetch resolves each sub's
// socket and meter from s.rw under m.mu (releaseRaw1 writes s.rw under it);
// only TCP_INFO runs unlocked. Under -race the old unlocked darkcallRead(s)
// raced this writer.
func TestFx247DcPrefetchRWUnderLock(t *testing.T) {
	old := *darkcallReadP.Load()
	defer darkcallReadSet(old)
	darkcallReadSet(darkcallReadOS)
	m := &mConn{}
	s := &msub{m: m, rw: deadSubRW{}}
	pre := []*msub{s}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3000; i++ {
			m.mu.Lock()
			if i%2 == 0 {
				s.rw = deadSubRW{}
			} else {
				s.rw = &mconnSub{}
			}
			m.mu.Unlock()
		}
	}()
	for i := 0; i < 3000; i++ {
		m.mu.Lock()
		m.dcPrefetchLocked(pre)
		if s.dcPre == nil || s.dcPre.tc != nil {
			m.mu.Unlock()
			t.Fatal("prefetch")
		}
		s.dcPre = nil
		m.mu.Unlock()
	}
	<-done
}

// darkcallReadSet keeps the OS flag right.
func TestFx247DarkcallReadSetFlag(t *testing.T) {
	old := *darkcallReadP.Load()
	defer darkcallReadSet(old)
	darkcallReadSet(func(*msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
		return nil, "", 0, 0, 0, 0, 0, false
	})
	if darkcallReadOSOn.Load() {
		t.Fatal("a test read taken as the OS one")
	}
	darkcallReadSet(darkcallReadOS)
	if !darkcallReadOSOn.Load() {
		t.Fatal("the OS read not recognised")
	}
}

// fx247 (audit246 L4): an ended conn leaves the sweeper at once.
func TestFx247EndLeavesSweeper(t *testing.T) {
	sw235Hooks(t, func(*msub) *net.TCPConn { return nil }, linkAck{})
	m, _ := sw235Conn(t, 4711, time.Second)
	if _, ok := subSweepBucketOf(m).Load(m); !ok {
		t.Fatal("not in the sweeper")
	}
	m.mu.Lock()
	m.subs = nil // no live subs to close (a bare test conn)
	m.die = make(chan struct{})
	m.mu.Unlock()
	m.end(errMConnClosed, false)
	obs.mconnOpen.Add(1) // a bare test conn was never counted open
	if _, ok := subSweepBucketOf(m).Load(m); ok || m.ksSwept {
		t.Fatal("an ended conn still in the sweeper")
	}
}

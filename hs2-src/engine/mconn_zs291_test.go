package engine

import (
	"testing"
	"time"
)

// zs291: an exit conn closed before any sub joined (serveMConnSub's lost
// reserve race: the raw first sub arriving after a rescue join made the
// conn) must end, so obs.mconnOpen does not creep (sk286: +86/h).
func TestZS291CloseNeverSubbedEnds(t *testing.T) {
	for _, raw := range []bool{true, false} {
		base := obs.mconnOpen.Load()
		var m *mConn
		if raw {
			m = newMConnExitRaw(0x291)
		} else {
			m = newMConnExit(0x292)
		}
		if got := obs.mconnOpen.Load(); got != base+1 {
			t.Fatalf("raw=%v open %d, want %d", raw, got, base+1)
		}
		m.Close()
		select {
		case <-m.GetDieCh():
		case <-time.After(2 * time.Second):
			t.Fatalf("raw=%v: closed conn with no sub never ended", raw)
		}
		if got := obs.mconnOpen.Load(); got != base {
			t.Fatalf("raw=%v open %d after Close, want %d", raw, got, base)
		}
		m.mu.Lock()
		err := m.err
		m.mu.Unlock()
		if err != errMConnClosed {
			t.Fatalf("raw=%v err %v", raw, err)
		}
	}
}

// A conn that has a sub is not counted as never-subbed by Close.
func TestZS291CloseWithSubNotCounted(t *testing.T) {
	_, iran, exit := newRawPair(t)
	before := zsNeverEnded.Load()
	exit.Close()
	iran.Close()
	if zsNeverEnded.Load() != before {
		t.Fatal("a conn with a sub counted as never-subbed")
	}
}

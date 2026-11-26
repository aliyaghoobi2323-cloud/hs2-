package engine

import (
	"testing"
	"time"
)

// l487: a call conn that is not in rrConns (lab3 d3D/d4D: a hedged records
// first sub, rscCap false) is never visited by darkcall, so its dark primary
// is never seen. With HS2_L487_CALLDETECT a call-like window registers it and
// the timer moves the primary off the dark sub.
func TestL487CallDetect(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			setDarkcall(t)
			holdDarkcallLoop(t)
			old := l487CallDetect.Load()
			l487CallDetect.Store(on)
			t.Cleanup(func() { l487CallDetect.Store(old) })
			_, iran, exit := darkswPairDC(t)
			// the miss: the call conn is not in the darkcall list
			rrConns.Delete(exit)
			exit.mu.Lock()
			p := exit.primary.Load()
			exit.callLikeAt = time.Now()
			exit.l487RegLocked() // what callRollLocked does on a call-like window
			reg := exit.l487Reg
			exit.mu.Unlock()
			if reg != on {
				t.Fatalf("registered %v with the key %v", reg, on)
			}
			f := newDcFake()
			darkcallReadSet(f.read)
			darkcallMarksSet(func(s *msub) (uint32, uint32, bool) { return 0, 0, true })
			f.set(p, "dark")
			sw0, dp0 := darkcallStat.switched.Load(), darkcallStat.darkPrim.Load()
			for i := 0; i < 4; i++ {
				exit.mu.Lock()
				exit.callLikeAt = time.Now()
				exit.mu.Unlock()
				darkcallTick(time.Now(), ctrlNow())
				time.Sleep(darkcallAfter() * 3 / 2)
			}
			exit.mu.Lock()
			newP := exit.primary.Load()
			exit.mu.Unlock()
			moved := newP != p
			if on && (!moved || darkcallStat.switched.Load() == sw0 || darkcallStat.darkPrim.Load() == dp0) {
				t.Fatalf("key on: dark primary not seen or not moved (moved %v)", moved)
			}
			if !on && moved {
				t.Fatal("key off: an unlisted conn was visited")
			}
			closeAndWait(t, iran, exit)
			if on {
				darkcallTick(time.Now(), ctrlNow()) // an ended conn leaves the list
				if _, ok := l487Conns.Load(exit); ok {
					t.Fatal("ended conn still listed")
				}
			}
		})
	}
}

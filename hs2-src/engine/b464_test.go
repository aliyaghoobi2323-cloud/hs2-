package engine

import (
	"testing"
	"time"
)

// b464: the hedge window knobs parse 0 as 0 (not the default); unset, empty,
// negative or junk give the default.
func TestB464HedgeEnv(t *testing.T) {
	const k = "HS2_B464_TEST_KNOB"
	cases := []struct {
		v    string
		set  bool
		want uint64
	}{
		{"", false, 77}, {"", true, 77}, {"0", true, 0}, {"4096", true, 4096},
		{"-1", true, 77}, {"x", true, 77}, {"8k", true, 77},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv(k, c.v)
		}
		if got := hedge3EnvU64(k, 77); got != c.want {
			t.Fatalf("%q set=%v: %d want %d", c.v, c.set, got, c.want)
		}
	}
}

// b464: the heavy rule and the early buffer follow the tuned window.
func TestB464HedgeWindow(t *testing.T) {
	setHedge4(t, true)
	ob, od := hedge3Bytes, hedge3Dur
	t.Cleanup(func() { hedge3Bytes, hedge3Dur = ob, od })

	hedge3Bytes, hedge3Dur = 4096, 100*time.Millisecond
	if hedgeEarlyBytes() != 4096 || hedgeEarlyDur() != 100*time.Millisecond {
		t.Fatalf("early window %d %v", hedgeEarlyBytes(), hedgeEarlyDur())
	}
	mc := newMConnExit(64)
	mc.mu.Lock()
	mc.lite, mc.h3 = true, true
	mc.sendOff = 4096
	if mc.hedge3HeavyLocked() {
		t.Fatal("heavy at the window")
	}
	mc.sendOff = 4097
	if !mc.hedge3HeavyLocked() {
		t.Fatal("not heavy past a 4 KiB window")
	}
	mc.mu.Unlock()
	mc.end(errMConnClosed, false) // no leaked conn loop (waitConnLoops in later tests)

	hedge3Bytes = 0 // 0 honoured: heavy after the first byte
	mc2 := newMConnExit(65)
	mc2.mu.Lock()
	mc2.lite, mc2.h3 = true, true
	if mc2.hedge3HeavyLocked() {
		t.Fatal("heavy before any byte")
	}
	mc2.sendOff = 1
	if !mc2.hedge3HeavyLocked() {
		t.Fatal("BYTES=0 not heavy after one byte")
	}
	mc2.mu.Unlock()
	mc2.end(errMConnClosed, false)
	if hedgeEarlyBytes() != 0 {
		t.Fatal("BYTES=0 early window not 0")
	}
}

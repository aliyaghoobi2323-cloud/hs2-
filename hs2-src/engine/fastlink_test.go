package engine

import (
	"testing"
	"time"
)

// withFastLink runs f with HS2_FAST_LINK set to on, restoring it after.
func withFastLink(t *testing.T, on bool) {
	t.Helper()
	old := fastLink
	fastLink = on
	t.Cleanup(func() { fastLink = old })
}

// Each sub-key turns only its own part off.
func TestFastLinkSubKeys(t *testing.T) {
	withFastLink(t, true)
	old := [3]bool{fastPauseOn, fastGateOn, fastInfoOn}
	t.Cleanup(func() { fastPauseOn, fastGateOn, fastInfoOn = old[0], old[1], old[2] })
	fastPauseOn, fastGateOn, fastInfoOn = false, true, true
	b := slotBackoffMin
	if d := slotRedialPause(time.Minute, &b); d == 0 {
		t.Fatal("PAUSE=0: a stable link must wait as without the key")
	}
	if !fastGate() || !fastInfo() {
		t.Fatal("PAUSE=0 turned another part off")
	}
	fastPauseOn, fastGateOn = true, false
	for i := 0; i < 200; i++ {
		if g := jitterGap(); g < 40*time.Millisecond {
			t.Fatalf("GATE=0: gap %v, want the default 40–160 ms", g)
		}
	}
	fastGateOn, fastInfoOn = true, false
	m, _, _ := newV2Manager(nil, 1, 4, false)
	m.gateInfo = true
	addManaged(m, newMeteredFake())
	m.exitInfo.Store(&peerInfo{V2: true})
	if _, _, ok := m.Pick(); ok {
		t.Fatal("INFO=0: a link with its own answer pending was picked")
	}
}

// Off: the redial pause is the old one — a link that lived 30 s resets the
// backoff to its floor and waits jitter(0.5 s); a shorter-lived one doubles it.
func TestSlotRedialPauseDefault(t *testing.T) {
	withFastLink(t, false)
	b := 2 * time.Second
	if d := slotRedialPause(slotStableAfter, &b); b != slotBackoffMin || d < slotBackoffMin/2 || d >= slotBackoffMin {
		t.Fatalf("stable link: pause %v backoff %v", d, b)
	}
	b = slotBackoffMin
	if d := slotRedialPause(10*time.Second, &b); b != 2*slotBackoffMin || d < slotBackoffMin || d >= 2*slotBackoffMin {
		t.Fatalf("10 s link (off): pause %v backoff %v, want doubled", d, b)
	}
	b = slotBackoffMax
	slotRedialPause(time.Second, &b)
	if b != slotBackoffMax {
		t.Fatalf("backoff passed its max: %v", b)
	}
}

// On: a link that lived fastStableAfter is redialed at once and resets the
// backoff; one that died right after the handshake still backs off.
func TestSlotRedialPauseFast(t *testing.T) {
	withFastLink(t, true)
	b := 4 * time.Second
	if d := slotRedialPause(fastStableAfter, &b); d != 0 || b != slotBackoffMin {
		t.Fatalf("5 s link: pause %v backoff %v, want 0 and the floor", d, b)
	}
	b = slotBackoffMin
	if d := slotRedialPause(time.Second, &b); b != 2*slotBackoffMin || d < slotBackoffMin {
		t.Fatalf("1 s link: pause %v backoff %v, want a doubled backoff", d, b)
	}
}

// On: a link whose own kindInfo answer is pending carries users when the pool
// already holds the exit's answer; with no answer in the pool it still waits.
// Off: it waits either way (TestPickWaitsForInfo).
func TestPickFastInfo(t *testing.T) {
	for _, on := range []bool{false, true} {
		withFastLink(t, on)
		m, _, _ := newV2Manager(nil, 1, 4, false)
		m.gateInfo = true
		addManaged(m, newMeteredFake())
		if _, _, ok := m.Pick(); ok {
			t.Fatalf("fast=%v: picked a link with no answer anywhere in the pool", on)
		}
		m.exitInfo.Store(&peerInfo{V2: true, Caps: capPortTags})
		_, rel, ok := m.Pick()
		if ok != on {
			t.Fatalf("fast=%v: pool knows the exit, picked=%v", on, ok)
		}
		if ok {
			rel()
		}
		// The last link gone clears the pool's answer: a new link waits again.
		m.exitInfo.Store(nil)
		if _, _, ok := m.Pick(); ok {
			t.Fatalf("fast=%v: picked after the pool's answer was cleared", on)
		}
	}
}

// The gate's spacing: 40–160 ms by default, 20–80 ms with the key.
func TestJitterGapFast(t *testing.T) {
	for _, c := range []struct {
		on     bool
		lo, hi time.Duration
	}{{false, 40 * time.Millisecond, 160 * time.Millisecond}, {true, 20 * time.Millisecond, 80 * time.Millisecond}} {
		withFastLink(t, c.on)
		for i := 0; i < 500; i++ {
			if g := jitterGap(); g < c.lo || g >= c.hi {
				t.Fatalf("fast=%v: gap %v outside [%v,%v)", c.on, g, c.lo, c.hi)
			}
		}
	}
	if pickInt(true, 16, 8) != 16 || pickInt(false, 16, 8) != 8 {
		t.Fatal("pickInt")
	}
}

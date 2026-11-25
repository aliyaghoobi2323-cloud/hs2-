package engine

import (
	"testing"
	"time"
)

// rc15b: a hedged conn is heavy only past hedge3Bytes plus the per-second
// allowance; the old cumulative rule with the key at 0.
func TestRC15bHeavyRate(t *testing.T) {
	oldH3, oldBps := liteHedge3, rc15HeavyBps
	defer func() { liteHedge3, rc15HeavyBps = oldH3, oldBps }()
	liteHedge3 = true
	rc15HeavyBps = 32 << 10

	mk := func(age time.Duration, sent uint64) *mConn {
		m := newMConn(91, false, nil)
		m.zsBorn = time.Now().Add(-age)
		m.h3 = true
		m.sendOff = sent
		return m
	}
	// a light long-lived conn: 8 KB in 1 s stays light (limit ~36 KB)
	m := mk(time.Second, 8<<10)
	m.mu.Lock()
	if m.hedge3HeavyLocked() {
		t.Fatal("8 KB in 1 s: heavy, want light")
	}
	m.mu.Unlock()
	// a burst: 64 KB in its first 100 ms is heavy, and stays heavy
	m = mk(100*time.Millisecond, 64<<10)
	m.mu.Lock()
	if !m.hedge3HeavyLocked() {
		t.Fatal("64 KB in 100 ms: light, want heavy")
	}
	m.sendOff = 0
	if !m.hedge3HeavyLocked() {
		t.Fatal("heavy not sticky")
	}
	m.mu.Unlock()
	// the receive direction counts too
	m = mk(100*time.Millisecond, 0)
	m.r.next = 64 << 10
	m.mu.Lock()
	if !m.hedge3HeavyLocked() {
		t.Fatal("64 KB received in 100 ms: light, want heavy")
	}
	m.mu.Unlock()
	// at or under hedge3Bytes: light whatever the age
	m = mk(0, hedge3Bytes)
	m.mu.Lock()
	if m.hedge3HeavyLocked() {
		t.Fatal("hedge3Bytes: heavy, want light")
	}
	m.mu.Unlock()
	// key 0: the old cumulative rule
	rc15HeavyBps = 0
	m = mk(time.Second, 8<<10)
	m.mu.Lock()
	if !m.hedge3HeavyLocked() {
		t.Fatal("key 0, 8 KB: light, want heavy (old rule)")
	}
	m.mu.Unlock()
}

// rc15b: b457PickTry never waits: a full cap says no at once.
func TestRC15bPickTry(t *testing.T) {
	defer b457SetPickCap(b457PickCapEnv(""))
	b457SetPickCap(0)
	if rel, ok := b457PickTry(); !ok {
		t.Fatal("cap off: not ok")
	} else {
		rel()
	}
	b457SetPickCap(1)
	rel, ok := b457PickTry()
	if !ok {
		t.Fatal("free slot: not ok")
	}
	t0 := time.Now()
	if _, ok2 := b457PickTry(); ok2 {
		t.Fatal("full cap: ok")
	}
	if time.Since(t0) > 50*time.Millisecond {
		t.Fatal("PickTry waited")
	}
	rel()
	rel2, ok := b457PickTry()
	if !ok {
		t.Fatal("released slot: not ok")
	}
	rel2()
}

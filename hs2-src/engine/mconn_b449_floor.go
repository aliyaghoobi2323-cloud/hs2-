package engine

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// b449c keep floor (HS2_B449_KEEP_FLOOR_KB, default 64; 0 = off): Iran's
// lost-copy reset fired after a peer-closed sub whose request bytes Iran
// had not kept — dropped by the write-path age trim without TCP proof
// (fx212 agegap: ~145 a second on t1 Iran) or refused over
// the 32 MiB global cap — while its other sub joined or missed a copy
// < mHoleSafe ago (holeRecent). A b449 move right after a dark open is
// exactly that window: the new sub joined with a hole by definition, the
// old one dies with a gap, no resend, errMConnLost although the peer may
// well have every byte. Now a connection's kept bytes up to the floor are
// dropped only on TCP proof (the 1 s look, subAgeTrimLookLocked sys=true)
// and are kept even over the global cap, so a small (request) connection
// never has an age or cap gap: its dying sub is resent instead. Memory: the
// floor is per connection and ends at b449KeepHard process-wide (t1 Iran
// 2026-11-20: sub kept max ~1 MB, 0 cap refusals, 99 % of kept writes < 64K).
// An atomic: tests change it while other tests' connections still run.
var b449KeepFloor atomic.Int64

func init() { b449KeepFloor.Store(int64(b449FloorEnv()) << 10) }

// b449FloorEnv: HS2_B449_KEEP_FLOOR_KB (0 turns the floor off; unset or bad: 64).
func b449FloorEnv() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_B449_KEEP_FLOOR_KB"))); err == nil && v >= 0 {
		return v
	}
	return 64
}

// b449KeepHard: past this process-wide the floor holds nothing: no keep over
// the global cap and no age hold (worst case 256 MiB, 1.5 % of t1 Iran RAM).
const b449KeepHard = 256 << 20

var b449FloorStat struct {
	ageHeld atomic.Uint64 // write-path age trims held back by the floor (no gap opened)
	capHeld atomic.Uint64 // writes kept over the global cap under the floor
}

// b449FloorKeepLocked: m may keep a write over subKeepGlobalMax (under m.mu).
func (m *mConn) b449FloorKeepLocked() bool {
	fl := b449KeepFloor.Load()
	if fl <= 0 || int64(m.keptBytes) >= fl || subResendStat.kept.Load() > b449KeepHard {
		return false
	}
	b449FloorStat.capHeld.Add(1)
	return true
}

// b449FloorTrimLocked: of the k aged chunks at the head of x.sent, how many
// the write-path trim (no TCP proof) may drop: only down to the floor.
func (m *mConn) b449FloorTrimLocked(x *msub, k int) int {
	fl := b449KeepFloor.Load()
	if fl <= 0 || subResendStat.kept.Load() > b449KeepHard { // bounded: past the ceiling the old trim
		return k
	}
	rest, j := int64(m.keptBytes), 0
	for j < k && rest > fl {
		rest -= int64(x.sent[j].c.n)
		j++
	}
	if j < k {
		b449FloorStat.ageHeld.Add(1)
	}
	return j
}

// rc14d floor fast (HS2_B449_FLOORFAST, default on; 0 = the old scan): lab1
// rc14d (12k users, 2026-11-24): the floor held ~16k write-path age trims a
// second on Iran ("keep floor 64K: ~3.29M age trims held" a run). A held
// chunk stays older than the keep age until the 1 s TCP look, so every write
// of that connection skipped the "nothing aged" shortcut and rescanned its
// aged chunks under m.mu only for the floor to keep them all. When the
// connection's kept bytes are at or under the floor, the floor keeps every
// chunk (b449FloorTrimLocked returns 0) and no gap heals off the TCP look, so
// the write path can stop before the scan: the same result, no scan.
var b449FloorFast atomic.Bool

func init() { b449FloorFast.Store(envBoolDefault("HS2_B449_FLOORFAST", true)) }

// b449FloorHoldsAllLocked (under m.mu): the write-path (no proof) age trim of
// m can drop nothing because of the floor.
func (m *mConn) b449FloorHoldsAllLocked() bool {
	if !b449FloorFast.Load() {
		return false
	}
	fl := b449KeepFloor.Load()
	return fl > 0 && int64(m.keptBytes) <= fl && subResendStat.kept.Load() <= b449KeepHard
}

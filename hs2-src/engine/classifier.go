package engine

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/blake2s"
)

// C1, the flow classifier and the user tag (REDESIGN §3.1): each flow (a
// user's TCP connection, or a TUN 5-tuple) gets a user tag, so the link
// scheduler (C2, sched.go) shares the link by user, and a class, so light
// traffic goes in the latency lane ahead of the bulk.
//
// The class is local and judged from behaviour, each classTick, over the
// bytes of the last 200 ms and 2 s in each direction:
//
//   - light while it moved less than classLightSpan over 2 s and less than
//     classLightBurst over 200 ms;
//   - unknown from its opening to its first tick; C2 queues the first
//     classNewBytes it sends meanwhile as light (fq_codel's credit for a new
//     sparse flow), the rest as bulk (queueClass);
//   - bulk otherwise, and once bulk it stays bulk until it is quiet, less
//     than classQuietBytes over 2 s (the "2 s of silence" of §3.1, with
//     room for an app's keepalives and acks: held to no byte at all, 54% of
//     the bulk verdicts on 20 minutes of the edge's traffic were flows that
//     only trickled; decision T1): a flow that went quiet after a burst
//     waits at most behind one bulk frame meanwhile (C2), while one that
//     pauses between chunks of a download does not get its next chunk into
//     the latency lane;
//   - confirmed light after two light evaluations in a row (≥ 200 ms): only
//     those are duplicated on a second link (C11, R2), never unknown ones.
//
// class_tx is this end's sending direction (C2's lane, C11); class_flow is
// bulk when either direction is (C5 placement, C9 striping).
//
// The hot path is one atomic add per write (flowStats.Add, where
// countedStream.Write has its own today); the tick reads the two counters
// and keeps 2 s of 100 ms slots, a bounded O(1) per flow. An idle flow costs
// nothing: a tick may skip it, and the next one ages its slots at once.

// Class is a flow's class.
type Class uint8

const (
	ClassUnknown Class = iota
	ClassLight
	ClassBulk
)

func (c Class) String() string {
	switch c {
	case ClassUnknown:
		return "unknown"
	case ClassLight:
		return "light"
	case ClassBulk:
		return "bulk"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// TuneClass 0 (cmd/hs2 applyTuning: HS2_TUNE_CLASS=0) turns C1 off: every
// flow counts as bulk (an empty latency lane; the scheduler still shares by
// user).
var TuneClass = 1

// ClassOff says whether C1 is off (HS2_TUNE_CLASS=0).
func ClassOff() bool { return TuneClass == 0 }

const (
	// classTick: a flow is judged this often.
	classTick = 100 * time.Millisecond
	// classSlots: the ticks of the 2 s window.
	classSlots = 20
	// classLightSpan, classLightBurst: light is less than this over 2 s and
	// less than that over the last two ticks (200 ms).
	classLightSpan  = 64 << 10
	classLightBurst = 16 << 10
	// classNewBytes: a flow not judged yet is queued as light for the first
	// this many bytes it sends.
	classNewBytes = 32 << 10
	// classQuietBytes: a bulk flow is light again once it moved less than
	// this over 2 s (the pacing net's quiet socket, wcNetQuietBytes; the
	// lead's decision T1, docs/architecture/wip/r1/decisions.fa.md): an
	// app's keepalives and acks never leave 2 s without a byte.
	classQuietBytes = 8 << 10
	// classV6Prefix: an IPv6 user is its address's /64 unless set otherwise
	// (a subscriber's privacy addresses rotate within it).
	classV6Prefix = 64
	// classSlotMax: a slot saturates here; past classLightBurst in a tick a
	// flow is bulk whatever the count, so no verdict reads it.
	classSlotMax = 1<<16 - 1
)

// UserTag is a user's tag: a keyed hash of its IPv4 address, or of the
// v6Prefix bits of its IPv6 address (<= 0 or past 128: /64). A v4-mapped
// address is its IPv4 one; a zone is dropped. 0 is no tag: an invalid
// address, never a valid one.
//
// The key (userTagSecret) is 16 random bytes made once per process and never
// sent: the tag goes to the other server in each stream's SYN (§3.1), and
// the unkeyed FNV-1a of round 1 let the exit read every user's IPv4 address
// back from it in milliseconds (the R1 review, part A, finding A3). The exit
// needs the tags only to tell users apart, and makes the tags of its own TUN
// packets from their inner addresses itself (l3_link.go): no tag is compared
// between the two ends, so a key of each process's own is enough, and the
// per-user fairness is the same. TuneTagKey 0 (HS2_TUNE_TAG_KEY=0) goes back
// to the unkeyed hash.
func UserTag(ip netip.Addr, v6Prefix int) uint32 {
	ip = ip.Unmap().WithZone("")
	var a [16]byte
	var b []byte
	switch {
	case ip.Is4():
		a4 := ip.As4()
		b = append(a[:0], a4[:]...)
	case ip.Is6():
		if v6Prefix <= 0 || v6Prefix > 128 {
			v6Prefix = classV6Prefix
		}
		p, err := ip.Prefix(v6Prefix)
		if err != nil {
			return 0
		}
		a = p.Addr().As16()
		b = a[:]
	default:
		return 0
	}
	var h uint32
	if TuneTagKey.Get() != 0 {
		h = keyedTag(b)
	} else {
		h = fnvAdd(fnvOffset, b)
	}
	if h == 0 {
		h = 1
	}
	return h
}

// TuneTagKey 1 (the default; HS2_TUNE_TAG_KEY): the user tag is keyed
// (UserTag); 0: the unkeyed FNV-1a of round 1, which reveals the address.
var TuneTagKey = newTuneInt(1)

// userTagSecret is the process's key of the user tags (never sent, never
// logged); userTagHash keeps keyed BLAKE2s-128 states of it (a TUN packet
// takes one per packet: no allocation on that path).
var (
	userTagMu     sync.RWMutex
	userTagSecret = newUserTagKey()
	userTagHash   = &sync.Pool{New: newUserTagHash}
)

func newUserTagKey() []byte {
	k := make([]byte, 16)
	rand.Read(k) // crypto/rand: never fails on Linux (Go 1.24+ panics rather than return short)
	return k
}

func newUserTagHash() any {
	userTagMu.RLock()
	defer userTagMu.RUnlock()
	h, err := blake2s.New128(userTagSecret)
	if err != nil {
		panic(err) // a 16-byte key is always valid
	}
	return &userTagState{h: h, key: userTagSecret}
}

type userTagState struct {
	h   hash.Hash
	key []byte // the key h was made with (a test's setUserTagKey drops stale ones)
	sum [blake2s.Size128]byte
}

// keyedTag is the first 32 bits of BLAKE2s-128 of b under the process's key.
func keyedTag(b []byte) uint32 {
	st := userTagHash.Get().(*userTagState)
	userTagMu.RLock()
	stale := &st.key[0] != &userTagSecret[0]
	userTagMu.RUnlock()
	if stale {
		st = newUserTagHash().(*userTagState)
	}
	st.h.Reset()
	st.h.Write(b)
	st.h.Sum(st.sum[:0])
	v := binary.BigEndian.Uint32(st.sum[:4])
	userTagHash.Put(st)
	return v
}

// setUserTagKey replaces the process's key (tests: a tag under another key).
func setUserTagKey(k []byte) {
	userTagMu.Lock()
	userTagSecret = append([]byte(nil), k...)
	userTagMu.Unlock()
}

// flowStats is a flow's payload bytes each way, cumulative: the hot path's
// only cost (Add), read by its classifier's tick.
type flowStats struct {
	tx, rx atomic.Uint64
}

// Add counts n bytes this end sent (tx) or received: one atomic add.
func (f *flowStats) Add(tx bool, n int) {
	if tx {
		f.tx.Add(uint64(n))
	} else {
		f.rx.Add(uint64(n))
	}
}

// classDir is one direction's window and verdict.
type classDir struct {
	last  uint64             // the counter at the latest tick
	sum   uint32             // the slots' sum (2 s)
	slots [classSlots]uint16 // bytes per tick, saturating
	cls   Class              // the latest verdict
	run   uint8              // light verdicts in a row (to 2)
}

// flowClassifier is one flow's state; only its ticker touches it.
type flowClassifier struct {
	d      [2]classDir // tx, rx
	at     int64       // the latest tick (now / classTick)
	pos    uint8       // the latest tick's slot
	ticked bool
	// class_tx's flips (the status line)
	toBulk, toLight uint32
	// rtHold: the realtime hold's ticks left (RTHold, rtlane.go)
	rtHold uint16
}

// Tick judges the flow at now (each classTick; a tick may skip an idle
// flow). classTx is this end's sending direction, classFlow the flow's
// (bulk if either way is), confirmedLight two light verdicts of classTx in
// a row. With C1 off every flow is bulk.
func (c *flowClassifier) Tick(f *flowStats, now time.Duration) (classTx, classFlow Class, confirmedLight bool) {
	k := int64(now / classTick)
	steps := int64(1)
	if c.ticked {
		steps = k - c.at
	}
	c.at, c.ticked = k, true
	cnt := [2]uint64{f.tx.Load(), f.rx.Load()}
	if ClassOff() {
		c.d[0].last, c.d[1].last = cnt[0], cnt[1]
		return ClassBulk, ClassBulk, false
	}
	// age the slots by the ticks since the latest (none: the same tick
	// again, its bytes join its slot)
	if steps >= classSlots {
		for j := range c.d {
			c.d[j].slots, c.d[j].sum = [classSlots]uint16{}, 0
		}
	} else {
		for range steps {
			c.pos = (c.pos + 1) % classSlots
			for j := range c.d {
				d := &c.d[j]
				d.sum -= uint32(d.slots[c.pos])
				d.slots[c.pos] = 0
			}
		}
	}
	prev := (c.pos + classSlots - 1) % classSlots
	was := c.d[0].cls
	for j := range c.d {
		d := &c.d[j]
		delta := cnt[j] - d.last
		d.last = cnt[j]
		old := uint32(d.slots[c.pos])
		v := uint32(min(uint64(old)+delta, classSlotMax))
		d.slots[c.pos] = uint16(v)
		d.sum += v - old
		d.judge(uint32(d.slots[c.pos])+uint32(d.slots[prev]), d.sum)
	}
	classTx = c.d[0].cls
	switch {
	case was == ClassLight && classTx == ClassBulk:
		c.toBulk++
	case was == ClassBulk && classTx == ClassLight:
		c.toLight++
	}
	classFlow = classTx
	if c.d[1].cls == ClassBulk {
		classFlow = ClassBulk
	}
	return classTx, classFlow, c.d[0].run >= 2
}

// judge is one direction's verdict over its last 200 ms (b200) and 2 s.
func (d *classDir) judge(b200, b2s uint32) {
	switch {
	case d.cls == ClassBulk && b2s >= classQuietBytes:
		// bulk until quiet: less than 8 KiB over 2 s
	case b2s < classLightSpan && b200 < classLightBurst:
		d.cls = ClassLight
	default:
		d.cls = ClassBulk
	}
	if d.cls != ClassLight {
		d.run = 0
	} else if d.run < 2 {
		d.run++
	}
}

// queueClass is the class C2 queues a flow's next frame by, its latest
// class_tx being c: a flow not judged yet is light for the first
// classNewBytes it sent and bulk past them; with C1 off, bulk.
func queueClass(c Class, f *flowStats) Class {
	switch {
	case ClassOff():
		return ClassBulk
	case c != ClassUnknown:
		return c
	case f.tx.Load() < classNewBytes:
		return ClassLight
	}
	return ClassBulk
}

// classLine is a link's status line (§3.1): its flows by class_tx, and the
// flips a minute.
func classLine(light, bulk, unknown int, toBulk, toLight float64) string {
	return fmt.Sprintf("classes: light %d / bulk %d / unknown %d per link; flips/min: light→bulk %.0f, bulk→light %.0f",
		light, bulk, unknown, toBulk, toLight)
}

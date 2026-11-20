package engine

import (
	"crypto/rand"
	"encoding/binary"
	"os"
	"sync/atomic"
	"time"
)

// Zero-downtime upgrade (ZDU, proposals/zdu350.md), building block B2: the
// process instance id.
//
// Every process has an instance {startNanos, rand}. With HS2_ZDU=1 it rides
// kindInfo as an optional trailing field (after the ext caps, peerinfo.go)
// together with ext bit 15 (capExtZDU). During a handover the peer sees links
// of two instances of the same server; the one with the newest startNanos is
// the current one, the others are draining (peerOld). The generation pin
// (B4), LinkManager counting (B3) and the exit pool (B5) build on this in
// later slots.
//
// HS2_ZDU is off by default: no ext bit 15 and no trailing field, so the
// kindInfo bytes are exactly the pre-ZDU ones (TestZDUWireUnchangedOff).

// capExtZDU: ext bit 15 (owner table: caps_ext.go). Announced only with
// HS2_ZDU=1; it says "my kindInfo carries an instance id and I keep my
// mconn subs inside one peer instance".
const capExtZDU = 1 << 15

// zduOn: HS2_ZDU=1 (both sides; default off, not in the envdef table).
var zduOn atomic.Bool

func init() { zduOn.Store(envBoolDefault("HS2_ZDU", false)) }

// zduInst is a process instance id. The zero value means "none" (a peer that
// did not send one: pre-ZDU or HS2_ZDU off).
type zduInst struct {
	Start uint64 // process start, unix nanoseconds
	Rand  uint32 // tells apart two processes started in the same nanosecond
}

func (a zduInst) known() bool { return a != zduInst{} }

// newer reports whether a is a later instance than b (startNanos first, then
// rand so that the order is total). A known instance is newer than none.
func (a zduInst) newer(b zduInst) bool {
	if a.Start != b.Start {
		return a.Start > b.Start
	}
	return a.Rand > b.Rand
}

// zduSelf is this process's instance, fixed at start. Wall clock on purpose:
// it orders two processes of one server, which a per-process monotonic
// reading cannot. An NTP step back between the two starts could invert the
// order, so the handover (slot S6) gives the child the parent's instance in
// HELLO and the child takes max(now, parent.Start+1).
var zduSelf = newZDUInst(time.Now())

func newZDUInst(now time.Time) zduInst {
	var r [4]byte
	if _, err := rand.Read(r[:]); err != nil {
		// never zero, never equal to a sibling started in the same ns
		binary.BigEndian.PutUint32(r[:], uint32(os.Getpid())|1)
	}
	in := zduInst{Start: uint64(now.UnixNano()), Rand: binary.BigEndian.Uint32(r[:])}
	if !in.known() {
		in.Rand = 1
	}
	return in
}

// zduInfoInst is the instance this server puts in kindInfo: zduSelf with
// HS2_ZDU=1, none otherwise.
func zduInfoInst() zduInst {
	if !zduOn.Load() {
		return zduInst{}
	}
	return zduSelf
}

// zduInfoExt is the ext bit this server announces for ZDU.
func zduInfoExt() uint32 {
	if !zduOn.Load() {
		return 0
	}
	return capExtZDU
}

// zdu reports whether the other server announced ZDU with an instance id.
// Both must be there: a bit without an id (a broken or cut message) is not
// trusted, nor an id without the bit.
func (pi *peerInfo) zdu() bool {
	return pi != nil && pi.V2 && pi.Ext&capExtZDU != 0 && pi.Inst.known()
}

// kindInfo trailing fields (after the ext caps): [tag u8][len u8][len bytes],
// repeated; an unknown tag is skipped, a field cut short ends the list.
const (
	infoTagInst    = 1  // zduInst: Start u64 BE, Rand u32 BE
	infoInstLen    = 12 // its value length
	infoTrailerMax = 2 + infoInstLen
)

// appendInfoTrailer appends this message's trailing fields.
func appendInfoTrailer(b []byte, pi peerInfo) []byte {
	if pi.Inst.known() {
		var v [2 + infoInstLen]byte
		v[0], v[1] = infoTagInst, infoInstLen
		binary.BigEndian.PutUint64(v[2:], pi.Inst.Start)
		binary.BigEndian.PutUint32(v[10:], pi.Inst.Rand)
		b = append(b, v[:]...)
	}
	return b
}

// readInfoTrailer reads the trailing fields in t into pi.
func readInfoTrailer(t []byte, pi *peerInfo) {
	for len(t) >= 2 {
		tag, n := t[0], int(t[1])
		if 2+n > len(t) {
			return
		}
		v := t[2 : 2+n]
		if tag == infoTagInst && n >= infoInstLen {
			pi.Inst = zduInst{Start: binary.BigEndian.Uint64(v), Rand: binary.BigEndian.Uint32(v[8:])}
		}
		t = t[2+n:]
	}
}

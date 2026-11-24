package censor

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"time"
)

// Event recorder and offline replay (step back §7.5). LAB ONLY.
//
// HS2_CENSOR_RECORD=<file> (read only when HS2_CENSOR_DETECT >= 1; default
// unset = off) makes the evaluator goroutine append every ring event it
// ingests and every tick time it steps, in the order it handled them, to
// <file>. The hooks are untouched (the recorder sits behind the ring, in the
// evaluator goroutine), so the key-off and hook zero-alloc rules hold. A
// write error turns the recorder off (one log line); the detector goes on.
//
// Format: fixed recSize-byte little-endian records. Each process start
// appends a header record (kind recHeader: magic, side, debug flag, start
// time), so one file may hold several process generations; replay starts a
// fresh evaluator at each header, as the live process did.
//
// replay feeds a recording through the current evaluator: the same events,
// in the same order, with the same tick times, so the verdict lines are the
// live ones whenever the rules are the same.

const (
	recSize   = 128
	recMagic  = 0x435a5231 // "CZR1"
	recHeader = 0
	recTick   = 6
)

type recorder struct {
	f   *os.File
	w   *bufio.Writer
	buf [recSize]byte
	err error
}

// openRecorder opens path for append; nil when it cannot.
func openRecorder(path string, side Side, debug bool, start time.Time, logf func(string, ...any)) *recorder {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logf("censor351: recorder off: %v", err)
		return nil
	}
	r := &recorder{f: f, w: bufio.NewWriterSize(f, 64<<10)}
	b := &r.buf
	*b = [recSize]byte{}
	b[0] = recHeader
	b[1] = uint8(side)
	if debug {
		b[2] = 1
	}
	binary.LittleEndian.PutUint64(b[8:], uint64(start.UnixNano()))
	binary.LittleEndian.PutUint64(b[48:], recMagic)
	r.put()
	r.flush()
	logf("censor351: recording the event stream to %s (lab only)", path)
	return r
}

func (r *recorder) put() {
	if r.err == nil {
		_, r.err = r.w.Write(r.buf[:])
	}
}

func (r *recorder) flush() {
	if r.err == nil {
		r.err = r.w.Flush()
	}
}

func putAddr(b []byte, a netip.Addr) (flags uint8) {
	if !a.IsValid() {
		return 0
	}
	x := a.As16()
	copy(b, x[:])
	if a.Is4() {
		return 3
	}
	return 1
}

func getAddr(b []byte, flags uint8) netip.Addr {
	if flags&1 == 0 {
		return netip.Addr{}
	}
	var x [16]byte
	copy(x[:], b)
	a := netip.AddrFrom16(x)
	if flags&2 != 0 {
		a = a.Unmap()
	}
	return a
}

func f64(b []byte, v float64) { binary.LittleEndian.PutUint64(b, math.Float64bits(v)) }
func rf64(b []byte) float64   { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }

// event appends one ring event.
func (r *recorder) event(ev *event) {
	b := &r.buf
	*b = [recSize]byte{}
	le := binary.LittleEndian
	b[0], b[1], b[2] = uint8(ev.kind), ev.stage, uint8(ev.front)
	b[3] = uint8(ev.d.Class)
	if ev.d.HaveTCP {
		b[4] = 1
	}
	if ev.p.Silent {
		b[5] = 1
	}
	b[6] = putAddr(b[16:32], ev.local) | putAddr(b[32:48], ev.remote)<<2
	b[7] = uint8(ev.d.Front)
	le.PutUint64(b[8:], uint64(ev.at))
	le.PutUint64(b[48:], ev.n)
	le.PutUint32(b[56:], ev.d.LifeMs)
	le.PutUint32(b[60:], ev.d.LastSentMs)
	le.PutUint32(b[64:], ev.d.LastRecvMs)
	le.PutUint32(b[68:], ev.d.MinRTTUs)
	le.PutUint64(b[72:], ev.d.Bytes)
	f64(b[80:], ev.p.Loss)
	f64(b[88:], ev.p.QueueMs)
	f64(b[96:], ev.p.PeerLoss)
	f64(b[104:], ev.p.RateBps)
	f64(b[112:], ev.p.PathLimited)
	le.PutUint16(b[120:], ev.p.BusyLinks)
	r.put()
}

// tick appends one evaluator step time and flushes (at most 5 s of the
// stream is lost when the process dies).
func (r *recorder) tick(now time.Time) {
	b := &r.buf
	*b = [recSize]byte{}
	b[0] = recTick
	binary.LittleEndian.PutUint64(b[8:], uint64(now.UnixNano()))
	r.put()
	r.flush()
}

// decode turns a record back into an event (kind evDial..evUsers).
func decode(b *[recSize]byte) event {
	le := binary.LittleEndian
	ev := event{kind: evKind(b[0]), stage: b[1], front: Front(b[2]), at: int64(le.Uint64(b[8:])), n: le.Uint64(b[48:])}
	ev.local = getAddr(b[16:32], b[6]&3)
	ev.remote = getAddr(b[32:48], b[6]>>2&3)
	switch {
	case ev.kind == evDeath || ev.kind == evGen && ev.stage == genOldEnd:
		ev.d = Death{Local: ev.local, Remote: ev.remote, Front: Front(b[7]), Class: DeathClass(b[3]),
			LifeMs: le.Uint32(b[56:]), Bytes: le.Uint64(b[72:]), HaveTCP: b[4] == 1,
			LastSentMs: le.Uint32(b[60:]), LastRecvMs: le.Uint32(b[64:]), MinRTTUs: le.Uint32(b[68:])}
	case ev.kind == evPath:
		ev.p = PathSample{Local: ev.local, Remote: ev.remote, Loss: rf64(b[80:]), QueueMs: rf64(b[88:]), Silent: b[5] == 1,
			PeerLoss: rf64(b[96:]), RateBps: rf64(b[104:]), BusyLinks: le.Uint16(b[120:]), PathLimited: rf64(b[112:])}
	}
	return ev
}

// replayStats counts what a replay fed.
type replayStats struct {
	gens, events, ticks int
}

// replay feeds a recording through fresh evaluators (one per header) and
// hands each log line to out with the time it was logged at (the tick
// time, or the event time for lines logged while ingesting). ispMap is the
// live HS2_CENSOR_ISPMAP (not recorded).
func replay(rd io.Reader, ispMap string, out func(at time.Time, line string)) (replayStats, error) {
	var st replayStats
	var b [recSize]byte
	var e *evaluator
	var cur time.Time
	logf := func(f string, a ...any) { out(cur, fmt.Sprintf(f, a...)) }
	br := bufio.NewReaderSize(rd, 1<<20)
	for {
		if _, err := io.ReadFull(br, b[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return st, nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return st, nil // a process killed mid-write: the partial record is dropped
			}
			return st, err
		}
		switch b[0] {
		case recHeader:
			if binary.LittleEndian.Uint64(b[48:]) != recMagic {
				return st, fmt.Errorf("bad header at record %d", st.events+st.ticks+st.gens)
			}
			start := time.Unix(0, int64(binary.LittleEndian.Uint64(b[8:])))
			cur = start
			e = newEvaluator(Side(b[1]), logf, start)
			e.ispMap = parseISPMap(ispMap)
			e.debug = b[2] == 1
			st.gens++
			logf("censor351: v1 detector on (level 1, side %s): log only, no reaction [replay]", sideName(Side(b[1])))
		case recTick:
			if e == nil {
				return st, errors.New("tick before header")
			}
			cur = time.Unix(0, int64(binary.LittleEndian.Uint64(b[8:])))
			st.ticks++
			e.step(cur)
		case uint8(evDial), uint8(evAccept), uint8(evDeath), uint8(evPath), uint8(evUsers), uint8(evPeerAbort), uint8(evPeerOrphan), uint8(evGen):
			if e == nil {
				return st, errors.New("event before header")
			}
			ev := decode(&b)
			cur = time.Unix(0, ev.at)
			st.events++
			e.ingest(ev)
		default:
			return st, fmt.Errorf("bad record kind %d", b[0])
		}
	}
}

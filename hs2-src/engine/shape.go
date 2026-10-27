package engine

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// shapedConn reshapes the on-wire TLS-record size distribution of a smux link.
//
// The mtcp path runs smux directly over the TLS connection, so without this the
// record sizes follow smux's own framing (large ~16 KiB data frames, tiny
// control frames) — a stable, tunnel-specific fingerprint. shapedConn sits
// between smux and the TLS conn and re-frames the byte stream so each write to
// TLS (hence each TLS record) has a size drawn from an HTTPS-like distribution
// (obfs.LengthSampler): mostly full-size records with a tail of small ones, the
// shape of ordinary bulk HTTPS.
//
// It is symmetric: every smux session is built through newSession, so both ends
// wrap their conn the same way — what one end frames, the other strips. The
// codec, under smux and inside TLS, is:
//
//	[dataLen:uint16][padLen:uint16][data:dataLen][pad:padLen]
//
// Large writes are split so each frame fills toward a sampled target (padLen≈0);
// a short tail (or a small standalone write) is padded UP to the sampled target,
// so real bytes — not just boundaries — match the target sizes. The decoder
// returns exactly data and discards pad, so the byte stream smux sees is
// unchanged. Because it changes the bytes on the wire, both peers must run a
// build that shapes (the tunnel already requires matched versions on both ends).
type shapedConn struct {
	net.Conn
	sampler *obfs.LengthSampler

	wmu  sync.Mutex
	wbuf []byte // reused frame-build scratch (write side)
	// carry (a link with C2's scheduler, whose writer flushes it): a write's
	// short tail is held, with the target sampled for its record, and goes
	// out at the head of the next write (filling that record) or at Flush
	// (padded to it). The smux writer writes a frame at a time, and a tail
	// padded per frame cost up to half a record per frame: ~3% of a link at
	// 16 KiB frames, a third at the 1 KiB ones of C2 on a slow link. Records
	// keep the sampled sizes, and a frame's end no longer shows in them.
	carry bool
	tail  []byte
	tgt   int // the target of the record the tail fills (0: none sampled)
	// count: the records are tallied (a QUIET link's, for its status line
	// and γ2's batching measure): records written, their data and pad bytes
	count              bool
	recs, dataB, padB  atomic.Uint64
	lineRecs, lineData uint64 // the tallies at the status line before (the runner's)
	linePad            uint64
	lineAt             time.Duration
	// lazy (a QUIET link, QuietLazy; γ2's batching): Flush holds the tail
	// while the socket has bytes unsent (lazy says how many), which it could
	// not pass anyway, and a timer flushes it once they are sent (each
	// shapeLazyCheck, shapeLazyMax at most): what comes meanwhile rides the
	// same record, at no cost of time. lazyAt: when the tail was first held
	// (zero: not held); lazyErr: a held tail's write error, for the next
	// Write or Flush; holds: tails held (the status line's)
	lazy    func() (int, error)
	lazyT   *time.Timer
	lazyAt  time.Time
	lazyErr error
	holds   atomic.Uint64

	rmu  sync.Mutex
	rbuf []byte // reused frame-read scratch (read side)
	rem  []byte // decoded data not yet handed to smux (slice into rbuf)
	hdr  [shapeHdrLen]byte
}

const (
	shapeHdrLen = 4
	// shapeMaxFrame bounds a single decoded frame, a defensive guard against a
	// corrupt/hostile length. Comfortably above the sampler's largest size.
	shapeMaxFrame = 32 << 10
	// shapeLazyCheck, shapeLazyMax: a held tail looks at the socket this
	// often, and goes this long after it was first held whatever it finds
	shapeLazyCheck = 250 * time.Microsecond
	shapeLazyMax   = 10 * time.Millisecond
)

func newShapedConn(c net.Conn, s *obfs.LengthSampler) *shapedConn {
	if s == nil {
		s = obfs.NewHTTPSLengthSampler()
	}
	return &shapedConn{Conn: c, sampler: s}
}

// Write splits p into sampler-sized frames and writes each as ONE conn write, so
// each becomes one TLS record of the sampled size. The final short chunk is
// padded up to its target. It returns len(p) on success (all bytes accepted).
func (c *shapedConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.carry {
		return c.writeCarry(p)
	}
	written := 0
	for len(p) > 0 {
		target := c.sampler.Sample()
		maxData := target - shapeHdrLen
		if maxData < 1 {
			maxData = 1
		}
		d := p
		if len(d) > maxData {
			d = d[:maxData]
		}
		pad := target - shapeHdrLen - len(d)
		if pad < 0 {
			pad = 0
		}
		need := shapeHdrLen + len(d) + pad
		if cap(c.wbuf) < need {
			c.wbuf = make([]byte, need)
		}
		f := c.wbuf[:need]
		f[0], f[1] = byte(len(d)>>8), byte(len(d))
		f[2], f[3] = byte(pad>>8), byte(pad)
		copy(f[shapeHdrLen:], d)
		// pad bytes: zero them (they are inside TLS, so unobservable and harmless)
		for i := shapeHdrLen + len(d); i < need; i++ {
			f[i] = 0
		}
		if _, err := c.Conn.Write(f); err != nil {
			return written, err
		}
		c.tally(len(d), pad)
		written += len(d)
		p = p[len(d):]
	}
	return written, nil
}

// writeCarry is Write in carry mode: full records of the sampled sizes, the
// held tail first; what is left short of a record is held (under wmu).
func (c *shapedConn) writeCarry(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if c.tgt == 0 {
			c.tgt = c.sampler.Sample()
		}
		need := max(c.tgt-shapeHdrLen, 1) - len(c.tail)
		if len(p) < need {
			c.tail = append(c.tail, p...)
			return n, nil
		}
		d := p[:need]
		if len(c.tail) > 0 {
			c.tail = append(c.tail, d...)
			d = c.tail
		}
		if err := c.record(d, c.tgt); err != nil {
			return n - len(p), err
		}
		p = p[need:]
		c.tail, c.tgt, c.lazyAt = c.tail[:0], 0, time.Time{}
	}
	return n, nil
}

// Flush writes the held tail (carry mode), padded to its record's target;
// lazy, it may hold it while the socket has bytes unsent.
func (c *shapedConn) Flush() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.lazyErr; err != nil {
		c.lazyErr = nil
		return err
	}
	if len(c.tail) == 0 {
		return nil
	}
	if c.lazy != nil && c.holdLocked() {
		return nil
	}
	return c.flushLocked()
}

// flushLocked writes the tail as a record (under wmu).
func (c *shapedConn) flushLocked() error {
	err := c.record(c.tail, c.tgt)
	c.tail, c.tgt, c.lazyAt = c.tail[:0], 0, time.Time{}
	return err
}

// holdLocked says whether the tail waits for the socket's unsent bytes,
// the timer armed (under wmu): none unsent (or no reading), or held
// shapeLazyMax already, it goes now.
func (c *shapedConn) holdLocked() bool {
	ns, err := c.lazy()
	if err != nil || ns <= 0 {
		return false
	}
	now := time.Now()
	switch {
	case c.lazyAt.IsZero():
		c.lazyAt = now
		c.holds.Add(1)
	case now.Sub(c.lazyAt) >= shapeLazyMax:
		return false
	}
	if c.lazyT == nil {
		c.lazyT = time.AfterFunc(shapeLazyCheck, c.lazyFire)
	} else {
		c.lazyT.Reset(shapeLazyCheck)
	}
	return true
}

// lazyFire is the held tail's timer: still held, it goes once the socket
// has sent what was ahead of it.
func (c *shapedConn) lazyFire() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if len(c.tail) == 0 || c.lazyAt.IsZero() {
		return // a write took it into a full record meanwhile
	}
	if c.holdLocked() {
		return
	}
	if err := c.flushLocked(); err != nil {
		c.lazyErr = err
	}
}

// record writes d as one record of target bytes (padded up to it).
func (c *shapedConn) record(d []byte, target int) error {
	pad := max(target-shapeHdrLen-len(d), 0)
	need := shapeHdrLen + len(d) + pad
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need)
	}
	f := c.wbuf[:need]
	f[0], f[1] = byte(len(d)>>8), byte(len(d))
	f[2], f[3] = byte(pad>>8), byte(pad)
	copy(f[shapeHdrLen:], d)
	clear(f[shapeHdrLen+len(d):])
	_, err := c.Conn.Write(f)
	if err == nil {
		c.tally(len(d), pad)
	}
	return err
}

// tally counts a record of data and pad bytes written (count).
func (c *shapedConn) tally(data, pad int) {
	if c.count {
		c.recs.Add(1)
		c.dataB.Add(uint64(data))
		c.padB.Add(uint64(pad))
	}
}

// recLine is a QUIET link's records line since the previous one (the
// runner's): records a second, data bytes a record, and the share of pad.
func (c *shapedConn) recLine(n int, now time.Duration) string {
	r, d, p := c.recs.Load(), c.dataB.Load(), c.padB.Load()
	dr, dd, dp := r-c.lineRecs, d-c.lineData, p-c.linePad
	el := time.Duration(0)
	if c.lineAt > 0 && now > c.lineAt {
		el = now - c.lineAt
	}
	c.lineRecs, c.lineData, c.linePad, c.lineAt = r, d, p, now
	rate, per, padShare := 0.0, 0.0, 0.0
	if el > 0 {
		rate = float64(dr) / el.Seconds()
	}
	if dr > 0 {
		per = float64(dd) / float64(dr)
	}
	if dd+dp > 0 {
		padShare = 100 * float64(dp) / float64(dd+dp)
	}
	return fmt.Sprintf("quiet records link %d: %.0f/s, data %.0f B a record, pad %.1f%% (all: %d records, %d B data, %d B pad, %d tails held)",
		n, rate, per, padShare, r, d, p, c.holds.Load())
}

// Read reassembles the original byte stream: it returns buffered decoded data
// first, and otherwise reads one frame, keeps its data, and discards its pad.
func (c *shapedConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.rem) == 0 {
		if _, err := io.ReadFull(c.Conn, c.hdr[:]); err != nil {
			return 0, err
		}
		dataLen := int(c.hdr[0])<<8 | int(c.hdr[1])
		padLen := int(c.hdr[2])<<8 | int(c.hdr[3])
		total := dataLen + padLen
		if total > shapeMaxFrame {
			return 0, io.ErrUnexpectedEOF
		}
		if cap(c.rbuf) < total {
			c.rbuf = make([]byte, total)
		}
		buf := c.rbuf[:total]
		if _, err := io.ReadFull(c.Conn, buf); err != nil {
			return 0, err
		}
		c.rem = buf[:dataLen] // hand back only the real data; pad is dropped
	}
	n := copy(p, c.rem)
	c.rem = c.rem[n:]
	return n, nil
}

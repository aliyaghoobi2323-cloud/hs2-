package engine

import (
	crand "crypto/rand"
	"io"
	mrand "math/rand/v2"
	"net"
	"os"
	"sync"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
	rng     *mrand.Rand        // per-link size draws (HS2_SHAPE_FAST), under wmu
	batch   tlscarrier.Batcher // the link socket's batching (HS2_SHAPE_FAST), or nil

	wmu  sync.Mutex
	wbuf []byte // reused frame-build scratch (write side)

	bulkState // HS2_SHAPE_BULK only, under wmu

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
)

// HS2_SHAPE_FAST=1 (tlscarrier.Batch) gives each link its own size generator
// and buffers its reads: measured on a CPU-bound sender and receiver, more
// Mbit/s per core with the same ping. Gathering a write's records into one
// system call (Cork/Flush) is HS2_SHAPE_CORK=1 on top, an experiment only: it
// cut CPU too, but at 1 Gbit/s it raised the ping of an interactive flow
// beside bulk ones (p99 74 → 143 ms, p=0.01 over 10 runs each), so it is off.
var shapeCork = os.Getenv("HS2_SHAPE_CORK") == "1"

// shapeSmall (HS2_SHAPE_SMALL=1) pads a small write to a small record
// (obfs.SmallTarget) instead of a bulk-sized one: the upload side of a
// download — window updates, keepalives — and interactive traffic stop
// costing ~1 KiB a message. Only the sender changes; the decoder takes any
// size, so one side may run it alone.
var shapeSmall = os.Getenv("HS2_SHAPE_SMALL") == "1"

func newShapedConn(c net.Conn, s *obfs.LengthSampler) *shapedConn {
	if s == nil {
		s = obfs.NewHTTPSLengthSampler()
	}
	sc := &shapedConn{Conn: c, sampler: s}
	if tlscarrier.Batch {
		// The tunnel's session starts here, on an authenticated link: from
		// now on its reads come in batches.
		if b := tlscarrier.BatcherOf(c); b != nil {
			b.BufferReads()
			if shapeCork {
				sc.batch = b
			}
		}
		var seed [32]byte
		crand.Read(seed[:])
		sc.rng = mrand.New(mrand.NewChaCha8(seed))
	}
	return sc
}

// Write splits p into sampler-sized frames and writes each as ONE conn write, so
// each becomes one TLS record of the sampled size. The final short chunk is
// padded up to its target. It returns len(p) on success (all bytes accepted).
func (c *shapedConn) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written, records, padded := 0, 0, 0
	defer func() { noteShapeWrite(records, written, padded) }()
	if c.batch != nil {
		// The records of this write leave together, in one system call (see
		// tlscarrier.batchConn); they are the same records either way. Until
		// the flush nothing has reached the socket, so a failed flush fails
		// the whole write.
		c.batch.Cork()
		defer func() {
			if ferr := c.batch.Flush(); ferr != nil && err == nil {
				n, err = 0, ferr
			}
		}()
	}
	// A write that fits one small record (a window update, a keepalive, a
	// stream header, a game datagram) is padded to a small size with
	// HS2_SHAPE_SMALL, not to a bulk-sized one.
	small := shapeSmall && len(p)+shapeHdrLen <= obfs.SmallMax
	if shapeBulk {
		// HS2_SHAPE_BULK (shape_bulk.go): full-size records like a web
		// server's. A small write takes the small path below, unless bytes
		// a bulk write left waiting go out with it in one record.
		if !small || len(c.pend) > 0 {
			n, err = c.writeBulk(p, &records, &padded)
			written = n
			return n, err
		}
	}
	for len(p) > 0 {
		var target int
		if small {
			target = obfs.SmallTarget(len(p)+shapeHdrLen, c.uniform(), c.uniform())
		} else {
			target = c.sample()
		}
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
		written += len(d)
		records++
		padded += shapeHdrLen + pad
		p = p[len(d):]
	}
	return written, nil
}

// sample draws the next record size: from the link's own fast generator
// when it has one (HS2_SHAPE_FAST), else from the shared sampler's
// crypto/rand — one getrandom(2) system call per record on kernels before
// 6.11, measured at ~14% of a busy sender's CPU. The sizes follow the same
// distribution either way; ChaCha8 is a cryptographic generator, seeded
// once per link from crypto/rand.
func (c *shapedConn) sample() int {
	if c.rng != nil {
		return c.sampler.At(c.rng.Float64())
	}
	return c.sampler.Sample()
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

// uniform is one draw in [0,1) from the link's generator, or crypto/rand.
func (c *shapedConn) uniform() float64 {
	if c.rng != nil {
		return c.rng.Float64()
	}
	return obfs.RandFloat()
}

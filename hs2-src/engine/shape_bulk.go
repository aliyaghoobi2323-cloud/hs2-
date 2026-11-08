package engine

import (
	"os"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// HS2_SHAPE_BULK=1: bulk data leaves in full-size TLS records, the way a web
// server sends a large file.
//
// Without it every non-small write is cut into records drawn from the
// obfs.LengthSampler (40–1400 bytes, ~1 KiB on average), so a download
// through the tunnel is a long run of ~1 KiB records with a spread of small
// ones — a pattern no web server serving a file produces. nginx (ssl_buffer_size
// 16k), Caddy and Go's net/http, and the large CDNs once a transfer is under
// way, send a large file as records of exactly 16 KiB of plaintext (16406
// bytes on the wire with TLS 1.3) with a short last one. With this key:
//
//   - a non-small write is cut into records of shapeBulkRecord bytes (the TLS
//     maximum), and only its tail is shorter;
//   - records continue across writes: smux writes a full data frame as 16 KiB
//     of data plus its 8-byte header, so cutting each write on its own would
//     give a full record and a 12-byte one, alternating. A write of at least
//     one full record keeps its tail (pend) for the next write — in a running
//     download, a few microseconds later — so the stream stays one full
//     record after another.
//     If no write follows within shapeBulkHold the tail is sent alone. Only
//     the tail of a write of 16 KiB or more waits, never a small or a
//     medium-sized write, and any write that comes sends the waiting bytes
//     at once, in front of its own: a game's or a call's data never waits;
//   - a tail is padded up to a sampled size as before (HS2_SHAPE_SMALL's
//     small sizes for a small one), so short records still carry no size of
//     their own.
//
// The pacing and the bursts of the records stay TCP's own, as on a web
// server: the tunnel adds no delay of its own and no cap. Go's TLS starts a
// connection with ~1 MSS records growing to 16 KiB over its first 128 KiB
// (crypto/tls and utls dynamic record sizing), as Google's and Cloudflare's
// servers do. Only the sender changes: the decoder takes any frame up to
// shapeMaxFrame, so one side may run it alone. Off, nothing here runs.
var shapeBulk = os.Getenv("HS2_SHAPE_BULK") == "1"

const (
	// shapeBulkRecord is one full record's plaintext: the TLS maximum
	// (2^14), the shaped frame header included.
	shapeBulkRecord = 16 << 10
	shapeBulkData   = shapeBulkRecord - shapeHdrLen
)

// shapeBulkHold: how long a held tail waits for the next write (a variable
// for tests only).
var shapeBulkHold = time.Millisecond

// bulkState is the write side's HS2_SHAPE_BULK state, guarded by wmu.
type bulkState struct {
	pend    []byte      // data held for the next record (< shapeBulkData)
	holdT   *time.Timer // sends pend if no write comes first
	holding bool        // holdT is armed
	werr    error       // a failed timer send: the link is dead
}

// writeBulk is Write under HS2_SHAPE_BULK for a non-small write, or a small
// one with bytes waiting. It reports p's bytes as written once they are in
// a record sent or in pend. Called with wmu held.
func (c *shapedConn) writeBulk(p []byte, records, padded *int) (int, error) {
	if c.werr != nil {
		return 0, c.werr
	}
	total := len(p)
	sent := 0 // bytes of p already in a record sent
	for len(c.pend)+len(p) >= shapeBulkData {
		k := shapeBulkData - len(c.pend)
		if err := c.emitBulk(p[:k], shapeBulkRecord, records, padded); err != nil {
			return sent, err
		}
		sent += k
		p = p[k:]
	}
	if len(c.pend)+len(p) == 0 {
		c.stopHold()
		return total, nil
	}
	// smux leaves 12 bytes a full frame; they grow by 12 a frame until
	// they fill a record of their own.
	if total >= shapeBulkData {
		c.pend = append(c.pend, p...)
		if c.holdT == nil {
			c.holdT = time.AfterFunc(shapeBulkHold, c.flushHeld)
		} else if !c.holding {
			c.holdT.Reset(shapeBulkHold)
		}
		c.holding = true
		return total, nil
	}
	c.stopHold()
	if err := c.emitBulk(p, c.tailTarget(len(c.pend)+len(p)+shapeHdrLen), records, padded); err != nil {
		return sent, err
	}
	return total, nil
}

// tailTarget is the record size of a short last record that needs `need`
// bytes: a small size for a small one (HS2_SHAPE_SMALL), else at least a
// draw from the bulk distribution, as the default shaper pads a tail.
func (c *shapedConn) tailTarget(need int) int {
	if shapeSmall && need <= obfs.SmallMax {
		return obfs.SmallTarget(need, c.uniform(), c.uniform())
	}
	return min(max(need, c.sample()), shapeBulkRecord)
}

// emitBulk sends pend followed by d as one frame padded to target (one TLS
// record) and empties pend. Called with wmu held.
func (c *shapedConn) emitBulk(d []byte, target int, records, padded *int) error {
	dl := len(c.pend) + len(d)
	pad := max(target-shapeHdrLen-dl, 0)
	need := shapeHdrLen + dl + pad
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need, max(need, shapeBulkRecord))
	}
	f := c.wbuf[:need]
	f[0], f[1] = byte(dl>>8), byte(dl)
	f[2], f[3] = byte(pad>>8), byte(pad)
	copy(f[shapeHdrLen+copy(f[shapeHdrLen:], c.pend):], d)
	clear(f[shapeHdrLen+dl:])
	c.pend = c.pend[:0]
	if _, err := c.Conn.Write(f); err != nil {
		return err
	}
	*records++
	*padded += shapeHdrLen + pad
	return nil
}

func (c *shapedConn) stopHold() {
	if c.holding {
		c.holdT.Stop()
		c.holding = false
	}
}

// flushHeld sends a held tail no write picked up within shapeBulkHold. A
// failed send kills the link: smux learns it from its next read or write.
func (c *shapedConn) flushHeld() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.holding = false
	if len(c.pend) == 0 || c.werr != nil {
		return
	}
	records, padded := 0, 0
	if err := c.emitBulk(nil, c.tailTarget(len(c.pend)+shapeHdrLen), &records, &padded); err != nil {
		c.werr = err
		c.Conn.Close()
		return
	}
	obs.shapeRecords.Add(uint64(records))
	obs.shapePad.Add(uint64(padded))
}

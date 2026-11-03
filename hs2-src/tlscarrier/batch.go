package tlscarrier

import (
	"net"
	"os"
	"sync"
)

// batchConn sits between TLS and the TCP socket of a link and lets the
// tunnel's length shaper (engine/shape.go) move many small TLS records with
// one system call each way. The shaper cuts a 16 KiB smux frame into ~17
// records of ~1 KiB; without this each record is its own write(2), and on the
// other side TLS reads each record with its own read(2) — measured, the
// system calls are over half of a busy link's CPU, encryption under a tenth.
//
//   - Writes: between Cork and Flush, what TLS writes is gathered and leaves
//     in one write at Flush. The records — their sizes and bytes — are the
//     ones TLS made; only how many reach the kernel per call changes (and so
//     the kernel fills whole segments instead of a full one plus a sliver
//     per record). Used only with HS2_SHAPE_CORK=1, an experiment: it raised
//     an interactive flow's ping beside bulk ones (see engine/shape.go).
//   - Reads: after BufferReads, the socket is read 32 KiB at a time and TLS
//     takes its records from that buffer.
//
// Both stay off until the tunnel's own session starts on an authenticated
// link (the engine calls them then): during the handshake and the first
// record TLS reads the socket itself, so what an active probe sees — how much
// of it is consumed, how the connection closes — is exactly crypto/tls's.
//
// Used only with HS2_SHAPE_FAST=1; otherwise links are built as before.
type batchConn struct {
	net.Conn

	wmu    sync.Mutex
	corked bool
	wbuf   []byte

	rbuf     []byte // nil until BufferReads
	rpos, rn int
}

// Batch: build links over a batchConn (HS2_SHAPE_FAST=1).
var Batch = os.Getenv("HS2_SHAPE_FAST") == "1"

const batchReadBuf = 32 << 10

// Batcher is what a batchConn offers the shaper.
type Batcher interface {
	Cork()
	Flush() error
	BufferReads()
}

func wrapBatch(c net.Conn) net.Conn {
	if !Batch {
		return c
	}
	return &batchConn{Conn: c}
}

// NetConn returns the socket (Carrier.TCPConn walks wrappers with it).
func (b *batchConn) NetConn() net.Conn { return b.Conn }

func (b *batchConn) Cork() {
	b.wmu.Lock()
	b.corked = true
	b.wmu.Unlock()
}

// Flush sends what was gathered since Cork in one write and ends the cork.
func (b *batchConn) Flush() error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	b.corked = false
	if len(b.wbuf) == 0 {
		return nil
	}
	_, err := b.Conn.Write(b.wbuf)
	b.wbuf = b.wbuf[:0]
	return err
}

func (b *batchConn) Write(p []byte) (int, error) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if !b.corked {
		return b.Conn.Write(p)
	}
	b.wbuf = append(b.wbuf, p...)
	return len(p), nil
}

// BufferReads turns on read buffering. Only the reader (TLS, one at a time)
// touches the read side, so it needs no lock; call it from that side or
// before reads begin.
func (b *batchConn) BufferReads() {
	if b.rbuf == nil {
		b.rbuf = make([]byte, batchReadBuf)
	}
}

func (b *batchConn) Read(p []byte) (int, error) {
	if b.rbuf == nil {
		return b.Conn.Read(p)
	}
	if b.rpos == b.rn {
		if len(p) >= len(b.rbuf) {
			return b.Conn.Read(p) // a read as big as the buffer: straight in
		}
		n, err := b.Conn.Read(b.rbuf)
		b.rpos, b.rn = 0, n
		if n == 0 {
			return 0, err
		}
	}
	n := copy(p, b.rbuf[b.rpos:b.rn])
	b.rpos += n
	return n, nil
}

// BatcherOf finds the batchConn under c (a TLS conn), or nil.
func BatcherOf(c net.Conn) Batcher {
	type netConner interface{ NetConn() net.Conn }
	for i := 0; i < 8 && c != nil; i++ {
		if b, ok := c.(*batchConn); ok {
			return b
		}
		nc, ok := c.(netConner)
		if !ok {
			return nil
		}
		c = nc.NetConn()
	}
	return nil
}

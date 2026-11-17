package engine

import (
	"io"
	"sync/atomic"
)

// mem64: a relay direction that waits for data holds no big buffer.
//
// io.CopyBuffer pins its 32 KB pooled buffer for the whole life of the
// connection, including the minutes an idle (keepalive, push) connection
// spends blocked in Read: 2 x 32 KB per side per live user connection, two
// thirds of the heap in the soak (7,000 live connections: ~440 MB on each
// side), and the RSS grows with every idle connection users keep open.
// copyLazy reads into a small per-direction buffer, takes a pooled 32 KB
// buffer only after a read fills the small one (data is streaming), and gives
// it back as soon as a read does not fill it (the reader drained what was
// queued), so a waiting direction pins relaySmall bytes. Only a burst that
// ends exactly on a buffer boundary waits with the big one (until its next
// read). A streaming direction pays one extra small read per drain.
// HS2_RELAY_LAZY=0 is the old io.CopyBuffer.
const relaySmall = 2 << 10

var relayLazy atomic.Bool

func init() { relayLazy.Store(envBoolDefault("HS2_RELAY_LAZY", true)) }

// relayCopy copies src to dst until either fails (the copy of both relays).
func relayCopy(dst io.Writer, src io.Reader) {
	if !relayLazy.Load() {
		bp := copyBufs.Get().(*[]byte)
		io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
		copyBufs.Put(bp)
		return
	}
	copyLazy(dst, src)
}

// copyLazy is io.CopyBuffer (no ReaderFrom/WriterTo) with the small/big
// buffer switch above. It returns the bytes copied.
func copyLazy(dst io.Writer, src io.Reader) (written int64) {
	small := make([]byte, relaySmall)
	var bp *[]byte
	defer func() {
		if bp != nil {
			copyBufs.Put(bp)
		}
	}()
	for {
		buf := small
		if bp != nil {
			buf = *bp
		}
		n, err := src.Read(buf)
		if n > 0 {
			nw, werr := dst.Write(buf[:n])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil || nw != n {
				return
			}
		}
		if err != nil {
			return
		}
		switch {
		case bp == nil && n == len(small):
			bp = copyBufs.Get().(*[]byte)
		case bp != nil && n < len(*bp):
			copyBufs.Put(bp)
			bp = nil
		}
	}
}

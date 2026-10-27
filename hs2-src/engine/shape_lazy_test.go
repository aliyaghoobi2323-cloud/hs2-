package engine

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// γ2's batching on a QUIET link (shape.go's lazy): while the socket has
// bytes unsent, a flushed tail waits for them and takes the messages that
// come meanwhile into its one record; once they are sent it goes (within a
// few shapeLazyCheck); with nothing unsent, or held shapeLazyMax, it goes
// at once; the tallies count the records and the tails held; a held tail's
// write error comes back at the next Flush.
func TestShapedConnLazy(t *testing.T) {
	var unsent atomic.Int64
	cc := &captureConn{}
	w := newShapedConn(cc, obfs.NewHTTPSLengthSampler())
	w.carry, w.count = true, true
	w.lazy = func() (int, error) { return int(unsent.Load()), nil }
	// nothing unsent: a flush writes its record at once
	w.Write(make([]byte, 40))
	if err := w.Flush(); err != nil || len(cc.snapshot()) != 1 || w.holds.Load() != 0 {
		t.Fatalf("idle socket: %v, %d records, %d held", err, len(cc.snapshot()), w.holds.Load())
	}
	// unsent bytes ahead: two messages ride one record
	unsent.Store(3000)
	w.Write(make([]byte, 40))
	if err := w.Flush(); err != nil || len(cc.snapshot()) != 1 {
		t.Fatalf("a busy socket's tail went: %v, %d records", err, len(cc.snapshot()))
	}
	w.Write(make([]byte, 40))
	w.Flush()
	time.Sleep(3 * shapeLazyCheck)
	if len(cc.snapshot()) != 1 || w.holds.Load() != 1 {
		t.Fatalf("held: %d records, %d held", len(cc.snapshot()), w.holds.Load())
	}
	unsent.Store(0)
	deadline := time.Now().Add(time.Second)
	for len(cc.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the held tail never went once the socket sent its bytes")
		}
		time.Sleep(shapeLazyCheck / 2)
	}
	if r, d := w.recs.Load(), w.dataB.Load(); r != 2 || d != 120 {
		t.Fatalf("%d records of %d data bytes; want 2 and 120 (two messages in one)", r, d)
	}
	// never past shapeLazyMax
	unsent.Store(5000)
	w.Write(make([]byte, 40))
	t0 := time.Now()
	w.Flush()
	for len(cc.snapshot()) < 3 {
		if time.Since(t0) > shapeLazyMax+500*time.Millisecond {
			t.Fatal("a tail held past shapeLazyMax")
		}
		time.Sleep(time.Millisecond)
	}
	if el := time.Since(t0); el < shapeLazyMax {
		t.Fatalf("held %v, less than shapeLazyMax with bytes unsent all along", el)
	}
	// a full record takes the held tail with it
	w.Write(make([]byte, 40))
	w.Flush()
	w.Write(make([]byte, 4000))
	w.wmu.Lock()
	held := w.lazyAt != (time.Time{})
	w.wmu.Unlock()
	if held {
		t.Fatal("a tail taken into a record still counts as held")
	}
	// a held tail's write error comes back
	unsent.Store(0)
	ec := &errConn{}
	w2 := newShapedConn(ec, obfs.NewHTTPSLengthSampler())
	w2.carry = true
	var u2 atomic.Int64
	u2.Store(100)
	w2.lazy = func() (int, error) { return int(u2.Load()), nil }
	w2.Write(make([]byte, 10))
	w2.Flush()
	u2.Store(0)
	time.Sleep(4 * shapeLazyCheck)
	if err := w2.Flush(); err == nil {
		t.Fatal("the held tail's write error was lost")
	}
}

type errConn struct{ captureConn }

func (c *errConn) Write(p []byte) (int, error) { return 0, errors.New("gone") }

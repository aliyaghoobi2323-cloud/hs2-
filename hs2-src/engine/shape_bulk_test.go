package engine

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// recConn keeps every Write (one record each), safe for the hold timer.
type recConn struct {
	net.Conn
	mu   sync.Mutex
	recs [][]byte
	fail bool
}

func (c *recConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return 0, errors.New("socket gone")
	}
	c.recs = append(c.recs, append([]byte(nil), p...))
	return len(p), nil
}
func (c *recConn) Close() error { return nil }
func (c *recConn) snap() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.recs...)
}

// withShapeBulk turns HS2_SHAPE_BULK on for one test, with this hold. The
// cleanup lets a pending hold timer fire before the keys change back.
func withShapeBulk(t *testing.T, small bool, hold time.Duration) {
	ob, osm, oh := shapeBulk, shapeSmall, shapeBulkHold
	shapeBulk, shapeSmall, shapeBulkHold = true, small, hold
	t.Cleanup(func() {
		time.Sleep(3 * hold)
		shapeBulk, shapeSmall, shapeBulkHold = ob, osm, oh
	})
}

// noShapeBulk pins HS2_SHAPE_BULK off for a test of the default shaper, so
// the suite also passes with the key on in the environment.
func noShapeBulk(t *testing.T) {
	ob := shapeBulk
	shapeBulk = false
	t.Cleanup(func() { shapeBulk = ob })
}

func decodeRecs(t *testing.T, recs [][]byte) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() { a.Write(bytes.Join(recs, nil)); a.Close() }()
	out, err := io.ReadAll(newShapedConn(b, nil))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// smux writes a full data frame as 16 KiB + its 8-byte header: a run of them
// becomes a run of full 16 KiB records, the leftover carried across writes,
// and the last bytes leave on their own within shapeBulkHold.
func TestShapeBulkFullRecords(t *testing.T) {
	// a hold long enough that a busy test machine never fires it between
	// two writes of this loop (in a download they are microseconds apart)
	withShapeBulk(t, true, 200*time.Millisecond)
	rc := &recConn{}
	sc := newShapedConn(rc, nil)
	var want []byte
	for i := 0; i < 200; i++ {
		p := make([]byte, 8+16<<10)
		crand.Read(p)
		want = append(want, p...)
		if n, err := sc.Write(p); n != len(p) || err != nil {
			t.Fatal(n, err)
		}
	}
	recs := rc.snap()
	for i, r := range recs {
		if len(r) != shapeBulkRecord {
			t.Fatalf("record %d of %d is %d bytes, want %d", i, len(recs), len(r), shapeBulkRecord)
		}
	}
	if len(recs) != len(want)/shapeBulkData {
		t.Fatalf("%d records for %d bytes", len(recs), len(want))
	}
	deadline := time.Now().Add(time.Second)
	for len(rc.snap()) == len(recs) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	recs = rc.snap()
	if last := len(recs[len(recs)-1]); last >= shapeBulkRecord {
		t.Fatalf("held tail not sent as a short record: %d", last)
	}
	if !bytes.Equal(decodeRecs(t, recs), want) {
		t.Fatal("the stream does not decode to what was written")
	}
}

// A small write (a game frame, a window update) sends the held bytes with it
// at once, in one small record, and is never held itself; a medium write is
// one record, padded to at least a bulk-distribution size.
func TestShapeBulkSmallNotHeld(t *testing.T) {
	withShapeBulk(t, true, 200*time.Millisecond)
	rc := &recConn{}
	sc := newShapedConn(rc, nil)
	big := bytes.Repeat([]byte{1}, 8+16<<10)
	sc.Write(big) // one full record, 12 bytes held
	if n := len(rc.snap()); n != 1 {
		t.Fatalf("%d records after one full frame", n)
	}
	game := bytes.Repeat([]byte{2}, 60)
	sc.Write(game)
	recs := rc.snap()
	if len(recs) != 2 || len(recs[1]) > obfs.SmallMax || len(recs[1]) < 12+60+shapeHdrLen {
		t.Fatalf("game write: %d records, last %d bytes", len(recs), len(recs[len(recs)-1]))
	}
	med := bytes.Repeat([]byte{3}, 5000)
	sc.Write(med)
	recs = rc.snap()
	if len(recs) != 3 || len(recs[2]) != 5000+shapeHdrLen {
		t.Fatalf("medium write: %d records, last %d bytes", len(recs), len(recs[len(recs)-1]))
	}
	sc.Write([]byte{4})
	time.Sleep(5 * shapeBulkHold)
	recs = rc.snap()
	if len(recs) != 4 {
		t.Fatalf("a small write alone: %d records", len(recs))
	}
	want := append(append(append(big, game...), med...), 4)
	if !bytes.Equal(decodeRecs(t, recs), want) {
		t.Fatal("the stream does not decode to what was written")
	}
}

// Writers racing the hold timer lose and duplicate nothing (run with -race).
func TestShapeBulkConcurrent(t *testing.T) {
	withShapeBulk(t, false, shapeBulkHold)
	rc := &recConn{}
	sc := newShapedConn(rc, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				n := []int{8 + 16<<10, 40, 3000, 32 << 10}[(i+g)%4]
				sc.Write(make([]byte, n))
				mu.Lock()
				got += n
				mu.Unlock()
				if i%50 == 0 {
					time.Sleep(2 * shapeBulkHold)
				}
			}
		}(g)
	}
	wg.Wait()
	time.Sleep(5 * shapeBulkHold)
	if out := decodeRecs(t, rc.snap()); len(out) != got {
		t.Fatalf("decoded %d bytes of %d", len(out), got)
	}
}

// A failed timer send fails the next write.
func TestShapeBulkHeldSendFails(t *testing.T) {
	withShapeBulk(t, true, shapeBulkHold)
	rc := &recConn{}
	sc := newShapedConn(rc, nil)
	sc.Write(make([]byte, 8+16<<10))
	rc.mu.Lock()
	rc.fail = true
	rc.mu.Unlock()
	time.Sleep(5 * shapeBulkHold)
	if _, err := sc.Write(make([]byte, 20000)); err == nil {
		t.Fatal("write after a failed held send succeeded")
	}
}

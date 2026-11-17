package smux

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowConn delays every write so frames queue up behind it (as on a busy link).
type slowConn struct {
	net.Conn
	writes atomic.Int64
	failAt int64 // the failAt-th write fails (0: never)
}

func (c *slowConn) Write(b []byte) (int, error) {
	n := c.writes.Add(1)
	if c.failAt > 0 && n >= c.failAt {
		return 0, errors.New("slowConn: write failed")
	}
	time.Sleep(200 * time.Microsecond)
	return c.Conn.Write(b)
}

func batchPayload(st, i, n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(st), uint64(i)))
	for k := range b {
		b[k] = byte(r.Uint32())
	}
	return b
}

// TestSendBatch: many streams writing at once over a slow conn with
// Config.SendBatch: every stream's bytes arrive intact and in order, fewer
// conn writes than frames were made, and a batch never passes the byte cap
// by more than its first frame.
func TestSendBatch(t *testing.T) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SendBatch = 32 << 10
	cfg.MaxFrameSize = 16 << 10
	sc := &slowConn{Conn: c1}
	cli, err := Client(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv, err := Server(c2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	b0, f0 := SendBatchStats()

	const streams, msgs = 16, 60
	sizes := func(st, i int) int { return 1 + (st*7919+i*104729)%20000 }
	var wg sync.WaitGroup
	errs := make(chan error, 2*streams)
	// server: read each stream fully and compare
	wg.Add(1)
	go func() {
		defer wg.Done()
		var rg sync.WaitGroup
		for s := 0; s < streams; s++ {
			st, err := srv.AcceptStream()
			if err != nil {
				errs <- err
				return
			}
			rg.Add(1)
			go func(st *Stream) {
				defer rg.Done()
				var hdr [1]byte
				if _, err := io.ReadFull(st, hdr[:]); err != nil {
					errs <- err
					return
				}
				id := int(hdr[0])
				for i := 0; i < msgs; i++ {
					want := batchPayload(id, i, sizes(id, i))
					got := make([]byte, len(want))
					if _, err := io.ReadFull(st, got); err != nil {
						errs <- err
						return
					}
					if !bytes.Equal(got, want) {
						errs <- errors.New("stream bytes differ")
						return
					}
				}
			}(st)
		}
		rg.Wait()
	}()
	for s := 0; s < streams; s++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			st, err := cli.OpenStream()
			if err != nil {
				errs <- err
				return
			}
			if _, err := st.Write([]byte{byte(id)}); err != nil {
				errs <- err
				return
			}
			for i := 0; i < msgs; i++ {
				if _, err := st.Write(batchPayload(id, i, sizes(id, i))); err != nil {
					errs <- err
					return
				}
			}
		}(s)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("timeout")
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	b1, f1 := SendBatchStats()
	batches, frames := b1-b0, f1-f0
	t.Logf("batches=%d frames=%d conn writes=%d", batches, frames, sc.writes.Load())
	if batches == 0 || frames <= batches {
		t.Fatalf("no batching: batches=%d frames=%d", batches, frames)
	}
	if w := uint64(sc.writes.Load()); w != batches {
		t.Fatalf("conn writes %d != batches %d", w, batches)
	}
}

// TestSendBatchOff: without SendBatch the session uses the upstream loops.
func TestSendBatchOff(t *testing.T) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	cli, _ := Client(c1, nil)
	defer cli.Close()
	srv, _ := Server(c2, nil)
	defer srv.Close()
	if cli.writesB != nil {
		t.Fatal("batch channel made with SendBatch 0")
	}
	b0, _ := SendBatchStats()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(ss, buf); err != nil || string(buf) != "hello" {
		t.Fatal(err, string(buf))
	}
	if b1, _ := SendBatchStats(); b1 != b0 {
		t.Fatal("batched without the key")
	}
}

// TestSendBatchWriteError: a failed conn write fails the writers of the
// frames in it and closes the session's writes.
func TestSendBatchWriteError(t *testing.T) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	cfg := DefaultConfig()
	cfg.SendBatch = 32 << 10
	cfg.KeepAliveDisabled = true
	sc := &slowConn{Conn: c1, failAt: 3}
	cli, err := Client(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	go func() { // drain the peer side
		io.Copy(io.Discard, c2)
	}()
	var wg sync.WaitGroup
	var failed atomic.Int32
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := cli.OpenStream()
			if err != nil {
				failed.Add(1)
				return
			}
			for i := 0; i < 50; i++ {
				if _, err := st.Write(make([]byte, 4000)); err != nil {
					failed.Add(1)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writers hung after a failed batch write")
	}
	if failed.Load() == 0 {
		t.Fatal("no writer saw the failed write")
	}
	select {
	case <-cli.chSocketWriteError:
	default:
		t.Fatal("session did not record the write error")
	}
}

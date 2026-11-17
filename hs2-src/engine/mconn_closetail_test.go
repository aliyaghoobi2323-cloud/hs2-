package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// fn164: serveUserMConn closed every sub (mconnEdge.end) right after
// mc.Close, before the writeLoops had written the queued tail and the FIN.
// The exit then saw its subs die without a FIN (errMConnSubsDied, or the
// rescue-rst hold) and the target got a truncated upload. The user here
// writes a tail and closes at once; the target must get every byte and a
// clean EOF.

// tailEmpty counts target connections that ended before their 16-byte header
// (a probe, or an upload lost whole); tailEmptyRst those of them reset.
var tailEmpty, tailEmptyRst atomic.Int64

type tailGot struct {
	id, want, got uint64
	sum           bool
	err           error
}

// tailSink: reads each connection to its end; the first 16 bytes are the
// connection id and the payload length, byte i of the payload is byte(i*7+id).
func tailSink(t *testing.T, out chan<- tailGot) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(20 * time.Second))
				var h [16]byte
				if _, err := io.ReadFull(c, h[:]); err != nil {
					tailEmpty.Add(1)
					if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
						tailEmptyRst.Add(1)
					}
					return // a probe that sent nothing (or an upload lost whole)
				}
				g := tailGot{id: binary.BigEndian.Uint64(h[:8]), want: binary.BigEndian.Uint64(h[8:]), sum: true}
				b := make([]byte, 32<<10)
				for {
					n, err := c.Read(b)
					for i := 0; i < n; i++ {
						if b[i] != byte((g.got+uint64(i))*7+g.id) {
							g.sum = false
						}
					}
					g.got += uint64(n)
					if err != nil {
						if err != io.EOF {
							g.err = err
						}
						break
					}
				}
				out <- g
			}()
		}
	}()
	return ln.Addr().String()
}

// startTunnelTarget is startTunnel with the exit's target given.
func startTunnelTarget(t *testing.T, min, max int, target string) string {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: target})
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    min, Max: max, PerLink: 50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
	})
	addr := "127.0.0.1:" + port
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("user port never opened")
	return ""
}

func sendTail(addr string, id uint64, size int) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	b := make([]byte, 16+size)
	binary.BigEndian.PutUint64(b, id)
	binary.BigEndian.PutUint64(b[8:], uint64(size))
	for i := 0; i < size; i++ {
		b[16+i] = byte(uint64(i)*7 + id)
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.Write(b)
	c.Close() // at once: the tail and the FIN are still queued in the tunnel
	return err
}

func TestMConnCloseRightAfterTail(t *testing.T) {
	for _, lite := range []bool{false, true} {
		name := "full"
		if lite {
			name = "lite"
		}
		t.Run(name, func(t *testing.T) {
			withMConn(t, true)
			setLite(t, lite)
			got := make(chan tailGot, 1024)
			addr := startTunnelTarget(t, 3, 3, tailSink(t, got))
			before := mconnStat.total.Load()
			for i := 0; mconnStat.total.Load() == before; i++ {
				if i == 50 {
					t.Fatal("no connection ever used an mconn")
				}
				sendTail(addr, 1<<40, 1024)
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			for len(got) > 0 {
				<-got
			}
			empty0, emptyRst0, rst0 := tailEmpty.Load(), tailEmptyRst.Load(), mconnTargetRst.Load()
			sizes := []int{100, 1500, 16 << 10, 64 << 10, 300 << 10, 1 << 20}
			const rounds = 8
			n := 0
			var wg sync.WaitGroup
			for r := 0; r < rounds; r++ {
				for _, sz := range sizes {
					n++
					wg.Add(1)
					go func(id uint64, sz int) {
						defer wg.Done()
						if err := sendTail(addr, id, sz); err != nil {
							t.Errorf("conn %d: write: %v", id, err)
						}
					}(uint64(n), sz)
				}
				wg.Wait()
			}
			bad := 0
			seen := map[uint64]bool{}
			dl := time.After(30 * time.Second)
			for len(seen) < n {
				select {
				case g := <-got:
					if g.id > uint64(n) || seen[g.id] {
						continue
					}
					seen[g.id] = true
					if g.got != g.want || !g.sum || g.err != nil {
						bad++
						t.Errorf("conn %d: target got %d of %d bytes (pattern ok %v), end err %v", g.id, g.got, g.want, g.sum, g.err)
					}
				case <-dl:
					var missing []uint64
					for id := uint64(1); id <= uint64(n); id++ {
						if !seen[id] {
							missing = append(missing, id)
						}
					}
					// fx171: say where the missing ones went. A target conn
					// that ended before its header is an upload lost whole
					// (the exit's mConn died with nothing delivered), not a
					// slow one.
					t.Fatalf("only %d of %d connections reached the target's end (%d bad); missing ids %v (sizes %v); target conns ended before the header: %d (%d reset); exit target resets %d",
						len(seen), n, bad, missing, missingSizes(missing, sizes),
						tailEmpty.Load()-empty0, tailEmptyRst.Load()-emptyRst0, mconnTargetRst.Load()-rst0)
				}
			}
			if bad > 0 {
				t.Fatalf("%d of %d uploads truncated or reset", bad, n)
			}
		})
	}
}

// missingSizes: the upload size of each missing id (ids count from 1 over
// rounds of sizes).
func missingSizes(ids []uint64, sizes []int) []int {
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = sizes[(id-1)%uint64(len(sizes))]
	}
	return out
}

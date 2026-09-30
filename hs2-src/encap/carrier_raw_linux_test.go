//go:build linux

package encap_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// The whole secure carrier (Noise IKpsk2 handshake, key confirmation,
// ChaCha20-Poly1305, replay window, adaptive FEC, pacing) runs unchanged over
// every raw encapsulation: these tests bring real carriers up over raw sockets
// in the private namespace TestMain created.

var rawKinds = []string{"icmp", "gre", "ipip", "ipx"}

func needNetns(t *testing.T) {
	t.Helper()
	if os.Getenv("HS2_ENCAP_NETNS") != "1" {
		t.Skip("needs root and a private network namespace")
	}
}

func shared() []byte { return bytes.Repeat([]byte{0x42}, 32) }

type pairT struct {
	cli, srv *udpcarrier.Conn
	ln       *udpcarrier.Listener
}

func carrierPair(t *testing.T, kind string, srvAddr, dialAddr string) pairT {
	t.Helper()
	ec := udpcarrier.EncapConfig{Kind: kind}
	ln, err := udpcarrier.ListenCfg(srvAddr, ec, shared(), 0)
	if err != nil {
		t.Fatalf("%s listen: %v", kind, err)
	}
	t.Cleanup(func() { ln.Close() })
	accCh := make(chan *udpcarrier.Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		c, _ := ln.Accept(ctx)
		accCh <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cli, err := udpcarrier.DialCfg(ctx, dialAddr, ec, shared(), 0)
	if err != nil {
		t.Fatalf("%s dial: %v", kind, err)
	}
	t.Cleanup(func() { cli.Close() })
	srv := <-accCh
	if srv == nil {
		t.Fatalf("%s: listener accepted nothing", kind)
	}
	t.Cleanup(func() { srv.Close() })
	return pairT{cli: cli, srv: srv, ln: ln}
}

func readFrame(t *testing.T, c *udpcarrier.Conn, d time.Duration) (byte, []byte) {
	t.Helper()
	type r struct {
		ft byte
		p  []byte
		e  error
	}
	ch := make(chan r, 1)
	go func() { ft, p, e := c.ReadFrame(); ch <- r{ft, p, e} }()
	select {
	case x := <-ch:
		if x.e != nil {
			t.Fatalf("ReadFrame: %v", x.e)
		}
		return x.ft, x.p
	case <-time.After(d):
		t.Fatalf("ReadFrame timed out")
	}
	return 0, nil
}

// frame builds a verifiable data frame: index, length and a digest of the body.
func frame(i, n int) []byte {
	b := make([]byte, n)
	binary.BigEndian.PutUint32(b, uint32(i))
	for j := 40; j < n; j++ {
		b[j] = byte(i*7 + j)
	}
	h := sha256.Sum256(b[40:])
	copy(b[4:36], h[:])
	return b
}

func checkFrame(p []byte) (int, bool) {
	if len(p) < 40 {
		return -1, false
	}
	h := sha256.Sum256(p[40:])
	return int(binary.BigEndian.Uint32(p)), bytes.Equal(h[:], p[4:36])
}

// A carrier over each raw encapsulation completes the handshake and moves
// full-size tunnel packets both ways, every byte verified.
func TestCarrierOverRawEncap(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			p := carrierPair(t, k, "127.0.0.1", "127.0.0.1:2096")
			const n = 400
			for dir, pair := range [][2]*udpcarrier.Conn{{p.cli, p.srv}, {p.srv, p.cli}} {
				from, to := pair[0], pair[1]
				done := make(chan map[int]bool, 1)
				go func() {
					got := map[int]bool{}
					deadline := time.After(10 * time.Second)
					for len(got) < n {
						type r struct {
							ft byte
							b  []byte
							e  error
						}
						ch := make(chan r, 1)
						go func() { ft, b, e := to.ReadFrame(); ch <- r{ft, b, e} }()
						select {
						case x := <-ch:
							if x.e != nil {
								done <- got
								return
							}
							if x.ft != core.TypeData {
								continue
							}
							i, ok := checkFrame(x.b)
							if !ok {
								t.Errorf("dir %d: corrupted frame", dir)
							}
							got[i] = true
						case <-deadline:
							done <- got
							return
						}
					}
					done <- got
				}()
				for i := 0; i < n; i++ {
					if err := from.SendFrame(core.TypeData, frame(i, 60+i%1200)); err != nil {
						t.Fatalf("send: %v", err)
					}
				}
				if got := <-done; len(got) != n {
					t.Fatalf("dir %d: delivered %d/%d frames", dir, len(got), n)
				}
			}
		})
	}
}

// Many links (the pool) from one host to one listener, each its own carrier,
// all live at once and demultiplexed by link id — what the autopilot relies on
// to run several links over a raw encapsulation.
func TestCarrierRawManyLinks(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			ec := udpcarrier.EncapConfig{Kind: k}
			ln, err := udpcarrier.ListenCfg("127.0.0.1", ec, shared(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			const links = 8
			srvs := make(chan *udpcarrier.Conn, links)
			go func() {
				for i := 0; i < links; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					c, err := ln.Accept(ctx)
					cancel()
					if err != nil {
						return
					}
					srvs <- c
				}
			}()
			var clis []*udpcarrier.Conn
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < links; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					c, err := udpcarrier.DialCfg(ctx, "127.0.0.1", ec, shared(), 0)
					if err != nil {
						t.Errorf("dial: %v", err)
						return
					}
					mu.Lock()
					clis = append(clis, c)
					mu.Unlock()
				}()
			}
			wg.Wait()
			defer func() {
				for _, c := range clis {
					c.Close()
				}
			}()
			if len(clis) != links {
				t.Fatalf("%d/%d links up", len(clis), links)
			}
			// Each client says its index; the matching server echoes it back on
			// the same carrier.
			byIdx := map[int]*udpcarrier.Conn{}
			for i, c := range clis {
				c.SendFrame(core.TypeData, []byte(fmt.Sprintf("link-%02d", i)))
			}
			for i := 0; i < links; i++ {
				s := <-srvs
				_, b := readFrame(t, s, 5*time.Second)
				var idx int
				fmt.Sscanf(string(b), "link-%02d", &idx)
				byIdx[idx] = s
				s.SendFrame(core.TypeData, append([]byte("echo:"), b...))
			}
			for i, c := range clis {
				_, b := readFrame(t, c, 5*time.Second)
				if want := fmt.Sprintf("echo:link-%02d", i); string(b) != want {
					t.Fatalf("link %d got %q, want %q (crossed links)", i, b, want)
				}
			}
			for _, s := range byIdx {
				s.Close()
			}
		})
	}
}

// A carrier whose peer uses another shared secret never comes up over a raw
// encapsulation: the listener gives it nothing (the framing magic drops it,
// and the handshake would too).
func TestCarrierRawWrongKey(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			ec := udpcarrier.EncapConfig{Kind: k}
			ln, err := udpcarrier.ListenCfg("127.0.0.1", ec, shared(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			if c, err := udpcarrier.DialCfg(ctx, "127.0.0.1", ec, bytes.Repeat([]byte{0x13}, 32), 0); err == nil {
				c.Close()
				t.Fatal("carrier came up with a wrong key")
			}
		})
	}
}

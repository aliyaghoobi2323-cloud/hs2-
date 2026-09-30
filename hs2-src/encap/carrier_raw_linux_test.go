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
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
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
	return carrierPairMTU(t, kind, srvAddr, dialAddr, 0)
}

// carrierPairMTU is carrierPair sized at a specific inner MTU (0 = the default),
// so a test can drive full-MTU packets over the raw path.
func carrierPairMTU(t *testing.T, kind, srvAddr, dialAddr string, innerMTU int) pairT {
	t.Helper()
	ec := udpcarrier.EncapConfig{Kind: kind}
	ln, err := udpcarrier.ListenCfg(srvAddr, ec, shared(), innerMTU)
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
	cli, err := udpcarrier.DialCfg(ctx, dialAddr, ec, shared(), innerMTU)
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
// tunnel packets both ways, every byte verified — including FULL inner-MTU
// frames, whose on-wire datagram approaches the path MTU, so raw-path bugs that
// only show at full size (header accounting, read-buffer sizing) are covered.
func TestCarrierOverRawEncap(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			innerMTU := udpcarrier.InnerMTUFor(k, 1500)
			p := carrierPairMTU(t, k, "127.0.0.1", "127.0.0.1:2096", innerMTU)
			const n = 400
			// Every third frame is full inner-MTU (the largest packet the tunnel
			// carries: ~1500 bytes on the wire once the carrier and encap headers
			// are added); the rest are the original small varied sizes.
			sizeOf := func(i int) int {
				if i%3 == 0 {
					return innerMTU
				}
				return 60 + i%1200
			}
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
					if err := from.SendFrame(core.TypeData, frame(i, sizeOf(i))); err != nil {
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
//
// Link ids are random 16-bit values with no uniqueness check (raw_linux.go's
// randLinkID), so two of the several links from this one IP can collide. That
// is a known production limitation, not a bug this test should hit: a colliding
// pair's second handshake is fed to the first link and fails, and the pool
// redials. The udpcarrier Conn does not expose its link id, so the test cannot
// pre-dedup; it retries the whole bring-up a bounded number of times. A genuine
// demux bug (a crossed reply) fails every attempt and so still fails the test.
func TestCarrierRawManyLinks(t *testing.T) {
	needNetns(t)
	const links = 8
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			for attempt := 0; ; attempt++ {
				if runRawManyLinks(t, k, links) {
					return
				}
				if attempt >= 4 {
					t.Fatalf("%s: could not bring up %d distinct links in 5 attempts", k, links)
				}
				t.Logf("%s: not all links came up (likely a link-id collision); retrying", k)
			}
		})
	}
}

// runRawManyLinks brings up `links` carriers from one IP to one listener over
// kind k and checks each is demultiplexed to its own carrier (its echo returns
// on the same link). It returns false if fewer than `links` came up — a link-id
// collision the caller retries; a crossed reply is a real bug and fails the test.
func runRawManyLinks(t *testing.T, k string, links int) bool {
	t.Helper()
	ec := udpcarrier.EncapConfig{Kind: k}
	ln, err := udpcarrier.ListenCfg("127.0.0.1", ec, shared(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
				return // a link-id collision loses one handshake; caller retries
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
		return false // a collision: not all links came up
	}
	// Each client says its index; the matching server echoes it back on the same
	// carrier. Any crossed delivery is a demux bug (t.Fatalf), not a collision.
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
	return true
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

// The carrier keys the raw framing magic with its shared secret, so two carriers
// on the same host and encapsulation but with DIFFERENT shared secrets use
// different magics and do not interfere. Observed AT THE SOCKET: a dialer keyed
// with the listener's secret reaches it, while one keyed with a different secret
// is dropped by the framing magic (the keyed BPF filter) before any handshake —
// not merely failing the handshake. (The carrier passes its shared secret as the
// encap framing key; udpcarrier's TestEncapFramingKeyedBySecret pins that
// mapping, which the socket layer here relies on.)
func TestRawFramingMagicKeyedBySecret(t *testing.T) {
	needNetns(t)
	secretA := shared()                       // the listener's secret
	secretB := bytes.Repeat([]byte{0x13}, 32) // a different tunnel's secret
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv, err := encap.Listen(k, "127.0.0.1", encap.Options{Key: secretA})
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer srv.Close()

			// Same secret: the framing magic matches, so the listener receives it.
			same, err := encap.Dial(k, "127.0.0.1", encap.Options{Key: secretA})
			if err != nil {
				t.Fatalf("dial (same secret): %v", err)
			}
			defer same.Close()
			if _, err := same.Write([]byte("same-secret")); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, 2048)
			srv.SetReadDeadline(time.Now().Add(2 * time.Second))
			if n, _, err := srv.ReadFrom(buf); err != nil || string(buf[:n]) != "same-secret" {
				t.Fatalf("%s: same-secret packet not received (n=%d err=%v)", k, n, err)
			}

			// Different secret: the magic differs, so the packets are dropped at
			// the socket (the keyed BPF magic) and never reach the listener.
			diff, err := encap.Dial(k, "127.0.0.1", encap.Options{Key: secretB})
			if err != nil {
				t.Fatalf("dial (other secret): %v", err)
			}
			defer diff.Close()
			for i := 0; i < 20; i++ {
				diff.Write([]byte("other-secret"))
			}
			srv.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
			if n, a, err := srv.ReadFrom(buf); err == nil {
				t.Fatalf("%s: a packet keyed with a different secret reached the listener: %q from %v", k, buf[:n], a)
			}
		})
	}
}

// maxIPv4Len returns the largest IPv4 total length captured for datagrams of the
// given IP protocol between loopback addresses. It locates the IPv4 header after
// the link-layer header (whatever its length) by scanning for a version-4 header
// of the protocol with 127.x source and destination, so it does not depend on
// the loopback pseudo-Ethernet header size.
func (s *sniffer) maxIPv4Len(proto byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := 0
	for _, f := range s.seen {
		for off := 0; off <= 18 && off+20 <= len(f); off++ {
			if f[off]>>4 != 4 || f[off+9] != proto {
				continue
			}
			if f[off+12] != 127 || f[off+16] != 127 { // loopback src/dst
				continue
			}
			total := int(f[off+2])<<8 | int(f[off+3])
			if total < 20 || off+total > len(f) {
				continue
			}
			if total > best {
				best = total
			}
			break
		}
	}
	return best
}

// rawIPProto is the IP protocol number each raw kind rides on (icmp 1, gre 47,
// ipip 4, ipx default 253) — what appears in the outer IPv4 header on the wire.
var rawIPProto = map[string]byte{"icmp": 1, "gre": 47, "ipip": 4, "ipx": 253}

// The MTU accounting is real, not just arithmetic: a full
// InnerMTUFor(kind,1500)-sized tunnel payload over each raw encapsulation leaves
// as an IPv4 datagram of no more than 1500 bytes, and of EXACTLY 20 (IPv4) +
// encap.Overhead(kind) + CarrierOverhead + innerMTU — captured off the wire with
// a packet sniffer. This is what keeps a full packet inside a 1500-byte path
// unfragmented over every encapsulation.
func TestCarrierRawOnWireMTU(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			innerMTU := udpcarrier.InnerMTUFor(k, 1500)
			sn := newSniffer(t)
			p := carrierPairMTU(t, k, "127.0.0.1", "127.0.0.1:2096", innerMTU)
			full := make([]byte, innerMTU)
			// Heavy parity so full-size parity shards ride the wire too.
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; i < 300; i++ {
					readFrame(t, p.srv, 5*time.Second)
					readFrame(t, p.cli, 5*time.Second)
				}
			}()
			for i := 0; i < 300; i++ {
				p.cli.SendFrame(core.TypeData, full)
				p.srv.SendFrame(core.TypeData, full)
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			time.Sleep(300 * time.Millisecond)

			proto, ok := rawIPProto[k]
			if !ok {
				t.Fatalf("no IP protocol known for kind %q", k)
			}
			maxIP := sn.maxIPv4Len(proto)
			if maxIP == 0 {
				t.Fatalf("%s: no IPv4 datagrams of protocol %d captured", k, proto)
			}
			want := 20 + encap.Overhead(k) + udpcarrier.CarrierOverhead + innerMTU
			if maxIP > 1500 {
				t.Errorf("%s: on-wire IPv4 datagram %d exceeds the 1500-byte path (would fragment)", k, maxIP)
			}
			if maxIP != want {
				t.Errorf("%s: largest on-wire IPv4 datagram %d, want 20+Overhead(%d)+CarrierOverhead(%d)+innerMTU(%d) = %d",
					k, maxIP, encap.Overhead(k), udpcarrier.CarrierOverhead, innerMTU, want)
			}
		})
	}
}

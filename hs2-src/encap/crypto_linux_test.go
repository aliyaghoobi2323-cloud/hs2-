//go:build linux

package encap_test

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// sniffer captures every frame on the loopback interface (AF_PACKET) so a test
// can inspect exactly what the carrier put on the wire.
type sniffer struct {
	fd   int
	mu   sync.Mutex
	seen [][]byte
	done chan struct{}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func newSniffer(t *testing.T) *sniffer {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatalf("AF_PACKET: %v", err)
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		unix.Close(fd)
		t.Fatalf("lo: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: lo.Index}); err != nil {
		unix.Close(fd)
		t.Fatalf("bind lo: %v", err)
	}
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 8<<20)
	unix.SetNonblock(fd, true)
	s := &sniffer{fd: fd, done: make(chan struct{})}
	go s.loop()
	t.Cleanup(func() { close(s.done); unix.Close(fd) })
	return s
}

func (s *sniffer) loop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		if n > 0 {
			s.mu.Lock()
			s.seen = append(s.seen, append([]byte(nil), buf[:n]...))
			s.mu.Unlock()
		}
	}
}

func (s *sniffer) contains(needle []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.seen {
		if bytes.Contains(f, needle) {
			return true
		}
	}
	return false
}

func (s *sniffer) frames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// Only ciphertext ever appears on the wire: a distinctive plaintext marker,
// sent as tunnel data over every encapsulation, is never found in any captured
// loopback frame — the datagram carrier's payload is confidential on the wire
// regardless of the encapsulation.
func TestOnWireCiphertextOnly(t *testing.T) {
	needNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			marker := bytes.Repeat([]byte{0xC0, 0xFF, 0xEE, 0x42, 0xDE, 0xAD, 0xBE, 0xEF}, 8)
			sn := newSniffer(t)
			p := carrierPair(t, k, "127.0.0.1", "127.0.0.1:2096")
			for i := 0; i < 100; i++ {
				p.cli.SendFrame(core.TypeData, marker)
				p.srv.SendFrame(core.TypeData, marker)
			}
			done := make(chan struct{})
			go func() {
				for i := 0; i < 100; i++ {
					readFrame(t, p.srv, 3*time.Second)
					readFrame(t, p.cli, 3*time.Second)
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			time.Sleep(200 * time.Millisecond)
			if sn.frames() == 0 {
				t.Fatal("sniffer captured no frames — cannot conclude anything")
			}
			if sn.contains(marker) {
				t.Fatalf("%s: plaintext marker found on the wire — payload is NOT encrypted", k)
			}
			t.Logf("%s: %d frames captured, no plaintext leak", k, sn.frames())
		})
	}
}

// A data datagram with ANY single byte of its sealed body flipped is rejected
// by the AEAD and never opens — the raw-encap framing creates no path around
// the carrier's integrity. This runs through core (what the carrier does after
// the encap header is stripped), so it is deterministic and exhaustive.
func TestSealedDatagramTamperTotal(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	server, _ := core.StaticFromSeed(secret, "hs2-udp-responder")
	client, _ := core.StaticFromSeed(secret, "hs2-udp-initiator")

	ini, err := core.NewInitiator(client, server.Public, secret)
	if err != nil {
		t.Fatal(err)
	}
	resp := core.NewResponder(server, secret)
	m1, _ := ini.WriteMessage1()
	hs, _, err := resp.ReadMessage1Payload(m1)
	if err != nil {
		t.Fatal(err)
	}
	m2, sSecret, err := resp.WriteMessage2(hs)
	if err != nil {
		t.Fatal(err)
	}
	cSecret, _, err := ini.ReadMessage2Payload(m2)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := core.NewSession(cSecret, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("top secret tunnel packet")
	seq, sealed, err := cs.SealDatagram(core.TypeData, 0, payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	ct := sealed[2:] // drop the masked length prefix, as the carrier does

	if ss, _ := core.NewSession(sSecret, false, 2); true {
		if _, _, got, err := ss.OpenDatagram(seq, ct); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("clean open failed: %v", err)
		}
	}
	for i := 0; i < len(ct); i++ {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x80
		ss, _ := core.NewSession(sSecret, false, uint64(100+i)) // fresh replay window each time
		if _, _, _, err := ss.OpenDatagram(seq, bad); err == nil {
			t.Fatalf("a datagram with sealed byte %d flipped still opened", i)
		}
	}
}

//go:build linux

package encap

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func listenT(t *testing.T, kind, addr string, opt Options) net.PacketConn {
	t.Helper()
	pc, err := Listen(kind, addr, opt)
	if err != nil {
		t.Fatalf("%s listen %s: %v", kind, addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc
}

func dialT(t *testing.T, kind, addr string, opt Options) net.Conn {
	t.Helper()
	c, err := Dial(kind, addr, opt)
	if err != nil {
		t.Fatalf("%s dial %s: %v", kind, addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readFromT(t *testing.T, pc net.PacketConn, d time.Duration) ([]byte, net.Addr) {
	t.Helper()
	buf := make([]byte, 2048)
	pc.SetReadDeadline(time.Now().Add(d))
	n, a, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	return buf[:n], a
}

func readT(t *testing.T, c net.Conn, d time.Duration) []byte {
	t.Helper()
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(d))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return buf[:n]
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Real raw sockets: every kind carries datagrams both ways, byte-exact, at
// sizes from empty to a full tunnel packet, with the link id preserved.
func TestRawSocketRoundTrip(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("rt-key")}
			srv := listenT(t, k, "127.0.0.1:2096", opt)
			cli := dialT(t, k, "127.0.0.1:2096", opt)
			cliID := cli.LocalAddr().(*Addr).ID
			for i, n := range []int{0, 1, 17, 512, 1400, 1472} {
				msg := bytes.Repeat([]byte{byte(i + 1)}, n)
				if _, err := cli.Write(msg); err != nil {
					t.Fatalf("write %d: %v", n, err)
				}
				got, from := readFromT(t, srv, 2*time.Second)
				if !bytes.Equal(got, msg) {
					t.Fatalf("c2s %d bytes: got %d bytes", n, len(got))
				}
				fa := from.(*Addr)
				if fa.ID != cliID || !fa.IP.Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatalf("peer address %v, want 127.0.0.1#%d", fa, cliID)
				}
				reply := append([]byte("re:"), msg...)
				if _, err := srv.WriteTo(reply, from); err != nil {
					t.Fatalf("reply: %v", err)
				}
				if got := readT(t, cli, 2*time.Second); !bytes.Equal(got, reply) {
					t.Fatalf("s2c %d bytes: got %d", len(reply), len(got))
				}
			}
		})
	}
}

// Several links from one IP are told apart by their link id, and a reply
// reaches only the link it is addressed to — the kernel filter drops the rest
// before they wake the other links' readers.
func TestRawSocketLinksDemux(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("demux")}
			srv := listenT(t, k, "127.0.0.1", opt)
			var clis []net.Conn
			addrs := map[string]net.Addr{}
			// Link ids are unique per (kind, peer IP) in this process.
			seenIDs := map[uint16]bool{}
			for i := 0; i < 4; i++ {
				c := dialT(t, k, "127.0.0.1", opt)
				if id := c.LocalAddr().(*Addr).ID; seenIDs[id] {
					t.Fatalf("%s: link id %d handed out twice", k, id)
				} else {
					seenIDs[id] = true
				}
				clis = append(clis, c)
				c.Write([]byte(fmt.Sprintf("link-%d", i)))
				got, from := readFromT(t, srv, 2*time.Second)
				addrs[string(got)] = from
			}
			if len(addrs) != 4 {
				t.Fatalf("expected 4 distinct links, got %v", addrs)
			}
			seen := map[string]bool{}
			for _, a := range addrs {
				seen[a.String()] = true
			}
			if len(seen) != 4 {
				t.Fatalf("links share an address: %v", seen)
			}
			// Reply only to link 2.
			srv.WriteTo([]byte("for-2"), addrs["link-2"])
			if got := readT(t, clis[2], 2*time.Second); string(got) != "for-2" {
				t.Fatalf("link 2 got %q", got)
			}
			for _, i := range []int{0, 1, 3} {
				clis[i].SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				buf := make([]byte, 2048)
				if n, err := clis[i].Read(buf); !isTimeout(err) {
					t.Fatalf("link %d got another link's packet (n=%d err=%v)", i, n, err)
				}
			}
		})
	}
}

// A dialer keyed with another secret never reaches the listener: its packets
// fail the framing magic (in the kernel filter already).
func TestRawSocketForeignKeyIgnored(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "127.0.0.1", Options{Key: []byte("ours")})
			foreign := dialT(t, k, "127.0.0.1", Options{Key: []byte("theirs")})
			for i := 0; i < 20; i++ {
				foreign.Write([]byte("hello?"))
			}
			srv.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			buf := make([]byte, 2048)
			if n, a, err := srv.ReadFrom(buf); !isTimeout(err) {
				t.Fatalf("listener accepted a foreign packet: %q from %v (%v)", buf[:n], a, err)
			}
		})
	}
}

// A wildcard listener answers from the local address each peer targeted, so
// a peer that dialed a secondary IP still accepts the reply (its connected
// socket takes nothing else) — the multi-IP server case.
func TestRawSocketWildcardRepliesFromTarget(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "0.0.0.0", Options{})
			cli := dialT(t, k, "127.0.0.2", Options{BindIP: "127.0.0.1"})
			cli.Write([]byte("ping"))
			_, from := readFromT(t, srv, 2*time.Second)
			srv.WriteTo([]byte("pong"), from)
			if got := readT(t, cli, 2*time.Second); string(got) != "pong" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// BindIP picks the source address the peer sees.
func TestRawSocketBindIP(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "127.0.0.1", Options{})
			cli := dialT(t, k, "127.0.0.1", Options{BindIP: "127.0.0.3"})
			cli.Write([]byte("x"))
			_, from := readFromT(t, srv, 2*time.Second)
			if ip := from.(*Addr).IP; !ip.Equal(net.IPv4(127, 0, 0, 3)) {
				t.Fatalf("source %v, want 127.0.0.3", ip)
			}
			if _, err := Dial(k, "127.0.0.1", Options{BindIP: "not-an-ip"}); err == nil {
				t.Fatal("bad bind IP accepted")
			}
		})
	}
}

// icmpReplySniffer counts ICMP echo REPLIES (type 0) to loopback seen on the
// wire, so a test can prove the kernel did not answer the tunnel's echo requests
// itself (the dialer's BPF filter drops a kernel echo — it carries the c2s
// magic — so the dialer's own read can never observe one).
type icmpReplySniffer struct {
	fd   int
	mu   sync.Mutex
	n    int
	done chan struct{}
}

func newICMPReplySniffer(t *testing.T) *icmpReplySniffer {
	t.Helper()
	hs := func(v uint16) uint16 { return v<<8 | v>>8 }
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(hs(unix.ETH_P_IP)))
	if err != nil {
		t.Fatalf("AF_PACKET: %v", err)
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		unix.Close(fd)
		t.Fatalf("lo: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: hs(unix.ETH_P_IP), Ifindex: lo.Index}); err != nil {
		unix.Close(fd)
		t.Fatalf("bind lo: %v", err)
	}
	unix.SetNonblock(fd, true)
	s := &icmpReplySniffer{fd: fd, done: make(chan struct{})}
	go s.loop()
	t.Cleanup(func() { close(s.done); unix.Close(fd) })
	return s
}

func (s *icmpReplySniffer) loop() {
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
		// Find the IPv4 header after the link-layer header (whatever its length),
		// then count an ICMP echo reply (protocol 1, type 0) between 127.x hosts.
		for off := 0; off <= 18 && off+20 <= n; off++ {
			if buf[off]>>4 != 4 || buf[off+9] != 1 { // IPv4 carrying ICMP
				continue
			}
			if buf[off+12] != 127 || buf[off+16] != 127 { // loopback src/dst
				continue
			}
			ihl := int(buf[off]&0x0f) * 4
			if off+ihl < n && buf[off+ihl] == icmpEchoReply {
				s.mu.Lock()
				s.n++
				s.mu.Unlock()
			}
			break
		}
	}
}

func (s *icmpReplySniffer) replies() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// The icmp listener keeps the kernel from answering the tunnel's echo requests,
// so the dialer hears only the listener — not its own datagrams echoed back. The
// listener here is a bare packet socket that sends no replies, so the ONLY ICMP
// echo replies (type 0) that could reach the wire are the kernel's answers to
// the dialer's echo requests; there must be none, whichever suppression method
// is in use. A sniffer confirms that directly: the dialer's read timing out
// proves nothing on its own (the dialer's BPF filter would drop a kernel echo).
func TestRawSocketICMPKernelSilent(t *testing.T) {
	needRawNetns(t)
	os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644)
	srv := listenT(t, KindICMP, "127.0.0.1", Options{})
	if m := echoGuardMethod(srv.(*rawPacketConn).guardKey); m == "" {
		t.Fatal("icmp listener holds no echo-reply suppression")
	}
	sn := newICMPReplySniffer(t)
	cli := dialT(t, KindICMP, "127.0.0.1", Options{})
	for i := 0; i < 10; i++ {
		cli.Write([]byte("req"))
	}
	for i := 0; i < 10; i++ {
		readFromT(t, srv, 2*time.Second)
	}
	time.Sleep(250 * time.Millisecond) // let any kernel echo reply reach the sniffer
	if n := sn.replies(); n != 0 {
		t.Fatalf("kernel emitted %d ICMP echo replies to the tunnel's requests", n)
	}
	cli.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := cli.Read(buf); !isTimeout(err) {
		t.Fatalf("dialer received %q without the listener answering (kernel echo?) err=%v", buf[:n], err)
	}
}

// readReplySeq reads one icmp reply off the dialer's raw socket and returns its
// echo sequence, failing the test on any trouble.
func readReplySeq(t *testing.T, rc *rawConn) uint16 {
	t.Helper()
	var tp []byte
	select {
	case bp := <-rc.q: // the link's queue holds the transport packet, header included
		tp = append([]byte(nil), *bp...)
	case <-time.After(2 * time.Second):
		t.Fatal("no reply")
	}
	_, seq, ok := rc.f.parse(tp)
	if !ok {
		t.Fatal("dialer rejected the listener's reply")
	}
	return seq
}

// Each reply carries the LISTENER's own echo-sequence counter, not the
// request's: consecutive replies to one peer get distinct, strictly ascending
// (mod 256) sequence low bytes, while the high byte stays the listener's s2c
// directional prefix. The ascending-per-reply counter is what keeps the stream
// looking like an ordinary ping and removes the repeated-(id,seq) tell (B4);
// the s2c prefix is how the echo guard tells hs2's genuine replies from the
// kernel's bounce of our own requests.
func TestRawSocketICMPReplySequence(t *testing.T) {
	needRawNetns(t)
	srv := listenT(t, KindICMP, "127.0.0.1", Options{})
	cli := dialT(t, KindICMP, "127.0.0.1", Options{})
	rc := cli.(*rawConn)
	sf := srv.(*rawPacketConn).f
	var prev uint16
	for i := 0; i < 5; i++ {
		cli.Write([]byte("q"))
		_, from := readFromT(t, srv, 2*time.Second)
		srv.WriteTo([]byte("a"), from)
		seq := readReplySeq(t, rc)
		if !obfSeqHasPrefix(seq, sf.txPrefix) {
			t.Fatalf("reply %d seq %#04x does not carry the listener's s2c prefix %#04x", i, seq, sf.txPrefix)
		}
		if ctr := seq & obfSeqCounterMask; i > 0 && (ctr-prev)&obfSeqCounterMask != 1 {
			t.Fatalf("reply %d counter %d is not +1 (mod 256) from the previous %d", i, ctr, prev)
		} else {
			prev = ctr
		}
	}
}

// N replies to one peer produce N distinct (id,seq) pairs: the reply counter is
// independent of the request sequence, so several replies between two requests —
// the exact case the old "answer with the request's sequence" collapsed onto one
// (id,seq) — now each get their own. Drive 300 replies (past one 256-wrap) from a
// single request and confirm every consecutive pair differs by one and the first
// full 256-run visits every low byte exactly once (B4).
func TestRawSocketICMPReplyCounterUnique(t *testing.T) {
	needRawNetns(t)
	srv := listenT(t, KindICMP, "127.0.0.1", Options{})
	cli := dialT(t, KindICMP, "127.0.0.1", Options{})
	rc := cli.(*rawConn)
	cli.Write([]byte("q")) // one request so the listener learns the peer
	_, from := readFromT(t, srv, 2*time.Second)
	const n = 300
	seqs := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		srv.WriteTo([]byte("a"), from)
		seqs = append(seqs, readReplySeq(t, rc))
	}
	for i := 1; i < n; i++ {
		if seqs[i] == seqs[i-1] {
			t.Fatalf("reply %d repeated the previous (id,seq): seq %#04x", i, seqs[i])
		}
		if d := (seqs[i] - seqs[i-1]) & obfSeqCounterMask; d != 1 {
			t.Fatalf("reply %d seq %#04x not +1 from %#04x (delta %d)", i, seqs[i], seqs[i-1], d)
		}
	}
	seen := map[uint16]bool{}
	for i := 0; i < 256; i++ {
		if c := seqs[i] & obfSeqCounterMask; seen[c] {
			t.Fatalf("counter %d repeated within one 256-run (at reply %d)", c, i)
		} else {
			seen[c] = true
		}
	}
	if len(seen) != 256 {
		t.Fatalf("expected 256 distinct counters in a full run, got %d", len(seen))
	}
}

//go:build linux

package engine

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b430: hs2's own close of a link that still holds unread data reaches the
// peer as an RST (Linux tcp_close), and is counted as an own abort on the
// local IP at the close; a close with nothing unread is a FIN (EOF at the
// peer) and is not counted.
func TestCensorOwnRstCloseCounted(t *testing.T) {
	for _, unread := range []bool{true, false} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		acc := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); acc <- c }()
		own, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		peer := <-acc
		ln.Close()
		tc := own.(*net.TCPConn)
		if unread {
			peer.Write(make([]byte, 3000))
			for i := 0; i < 200 && censorUnread(tc) < 3000; i++ {
				time.Sleep(5 * time.Millisecond)
			}
			if n := censorUnread(tc); n != 3000 {
				t.Fatalf("SIOCINQ %d want 3000", n)
			}
		} else if n := censorUnread(tc); n != 0 {
			t.Fatalf("SIOCINQ %d want 0", n)
		}
		ip := netip.MustParseAddr("127.0.0.1")
		before := ownAbortsOn(ip)
		st0 := censor.PeerCounters()
		got := censorCountRstClose(tc)
		own.Close()
		if got != unread {
			t.Fatalf("unread=%v: counted %v", unread, got)
		}
		if d := ownAbortsOn(ip) - before; d != map[bool]uint32{true: 1, false: 0}[unread] {
			t.Fatalf("unread=%v: per-IP own aborts +%d", unread, d)
		}
		st := censor.PeerCounters()
		if d := st.OwnRstCloses - st0.OwnRstCloses; d != map[bool]uint64{true: 1, false: 0}[unread] {
			t.Fatalf("unread=%v: own_rst_closes +%d", unread, d)
		}
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = io.ReadAll(peer)
		peer.Close()
		if unread && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("peer after a close with unread data: %v, want ECONNRESET (the premise)", err)
		}
		if !unread && err != nil {
			t.Fatalf("peer after a clean close: %v, want EOF", err)
		}
	}
	if censorUnread(nil) != -1 {
		t.Fatal("nil conn")
	}
}

// b430: watchConn.Close (hs2's own close; smux Session.Close goes through it)
// runs the own-close check once and only when no socket error came first; the
// detector off (the default) costs nothing and counts nothing.
func TestCensorOwnCloseOnlyOnce(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	w := &watchConn{Conn: a, raw: a, onErr: func() {}}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if censor.Active() && censorIsEdge.Load() && !w.ownClosed.Load() {
		t.Fatal("own close not marked")
	}
	w2 := &watchConn{Conn: b, raw: b, onErr: func() {}}
	w2.fail("read", io.EOF)
	w2.Close()
	if w2.ownClosed.Load() {
		t.Fatal("a close after a socket error ran the own-close check")
	}
}

func ownAbortsOn(ip netip.Addr) uint32 {
	if v, ok := censorAborts.Load(ip); ok {
		return v.(*ownAborts).n.Load()
	}
	return 0
}

// o432: a FIN close counts as an own abort only when the path has stopped
// answering (our segments unacked through 2+ RTO backoffs); a healthy or
// merely lossy socket, or no TCP_INFO, does not.
func TestCensorOwnStallClose(t *testing.T) {
	for _, c := range []struct {
		st   tcpStat
		ok   bool
		want bool
	}{
		{tcpStat{unacked: 3, backoff: 2}, true, true},
		{tcpStat{unacked: 1, backoff: 6}, true, true},
		{tcpStat{unacked: 3, backoff: 1}, true, false}, // one RTO: lossy, not stalled
		{tcpStat{unacked: 0, backoff: 4}, true, false}, // nothing of ours unanswered
		{tcpStat{unacked: 3, backoff: 4}, false, false},
	} {
		if got := ownCloseStalled(c.st, c.ok); got != c.want {
			t.Fatalf("%+v ok=%v: %v want %v", c.st, c.ok, got, c.want)
		}
	}
	// a healthy loopback link: not stalled, nothing counted
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	own, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer := <-acc
	defer peer.Close()
	defer own.Close()
	st0 := censor.PeerCounters()
	if censorCountStallClose(own.(*net.TCPConn)) || censorCountStallClose(nil) {
		t.Fatal("healthy socket counted as a stalled close")
	}
	if censor.PeerCounters().OwnStallCloses != st0.OwnStallCloses {
		t.Fatal("own_stall_closes moved")
	}
}

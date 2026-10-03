//go:build linux

package encap

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// 300 links to one peer share ONE dial socket (a received packet costs the
// same with 1 link as with 300), every link gets its own id, a reply reaches
// only its link, and the socket closes with the last link.
func TestRawMuxSharesOneSocket(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("mux")}
			srv := listenT(t, k, "127.0.0.1", opt)
			const n = 300
			clis := make([]net.Conn, n)
			ids := map[uint16]bool{}
			for i := range clis {
				c, err := Dial(k, "127.0.0.1", opt)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				clis[i] = c
				id := c.LocalAddr().(*Addr).ID
				if ids[id] {
					t.Fatalf("link id %d handed out twice", id)
				}
				ids[id] = true
			}
			if s, _ := rawMuxStats(); s != 1 {
				t.Fatalf("%d links opened %d dial sockets, want 1", n, s)
			}
			// Every link sends; the listener answers each one by its address.
			from := map[string]net.Addr{}
			for i, c := range clis {
				c.Write([]byte(fmt.Sprintf("q%d", i)))
				got, a := readFromT(t, srv, 2*time.Second)
				from[string(got)] = a
			}
			for _, i := range []int{0, 7, 150, 299} {
				srv.WriteTo([]byte(fmt.Sprintf("a%d", i)), from[fmt.Sprintf("q%d", i)])
			}
			for _, i := range []int{0, 7, 150, 299} {
				if got := readT(t, clis[i], 2*time.Second); string(got) != fmt.Sprintf("a%d", i) {
					t.Fatalf("link %d got %q", i, got)
				}
			}
			for _, i := range []int{1, 8, 151, 298} {
				clis[i].SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				if n, err := clis[i].Read(make([]byte, 64)); !isTimeout(err) {
					t.Fatalf("link %d got another link's reply (n=%d err=%v)", i, n, err)
				}
			}
			for _, c := range clis {
				c.Close()
			}
			if s, _ := rawMuxStats(); s != 0 {
				t.Fatalf("%d dial sockets left open after every link closed", s)
			}
		})
	}
}

// A link whose reader stalls loses its own packets once its queue is full;
// the other links on the shared socket keep receiving.
func TestRawMuxStalledLinkDoesNotBlockOthers(t *testing.T) {
	needRawNetns(t)
	opt := Options{Key: []byte("stall")}
	srv := listenT(t, KindGRE, "127.0.0.1", opt)
	stalled := dialT(t, KindGRE, "127.0.0.1", opt)
	live := dialT(t, KindGRE, "127.0.0.1", opt)
	stalled.Write([]byte("s"))
	_, sa := readFromT(t, srv, 2*time.Second)
	live.Write([]byte("l"))
	_, la := readFromT(t, srv, 2*time.Second)
	for i := 0; i < rawMuxQueue+200; i++ {
		srv.WriteTo([]byte("x"), sa) // nobody reads the stalled link
	}
	srv.WriteTo([]byte("for-live"), la)
	if got := readT(t, live, 2*time.Second); string(got) != "for-live" {
		t.Fatalf("live link got %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, d := rawMuxStats(); d > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no drops counted for the stalled link's overflow")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The stalled link still has its queued packets when it reads again.
	if got := readT(t, stalled, time.Second); string(got) != "x" {
		t.Fatalf("stalled link got %q", got)
	}
}

// Closing a link wakes its blocked reader and frees its id; a read deadline
// set while blocked takes effect.
func TestRawMuxCloseAndDeadline(t *testing.T) {
	needRawNetns(t)
	opt := Options{Key: []byte("close")}
	listenT(t, KindIPIP, "127.0.0.1", opt)
	c := dialT(t, KindIPIP, "127.0.0.1", opt)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 64))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	select {
	case err := <-errc:
		if !isTimeout(err) {
			t.Fatalf("blocked read ended with %v, want a timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a deadline set during a blocked read did not wake it")
	}
	go func() {
		c.SetReadDeadline(time.Time{})
		_, err := c.Read(make([]byte, 64))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case err := <-errc:
		if err == nil || isTimeout(err) {
			t.Fatalf("read after close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wake a blocked read")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("write on a closed link succeeded")
	}
}

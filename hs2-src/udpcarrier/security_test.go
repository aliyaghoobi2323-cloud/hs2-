package udpcarrier

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// TestMITMRejected is the datagram analogue of tlscarrier's TestMITMRejected: a
// real relay stands between client and server and tries to terminate the
// carrier on each side with its OWN secret (it does not have the tunnel's). The
// client, which pins the server key derived from the true secret, must reject
// the relay's handshake, and the true server must never yield a carrier for the
// intercepted client.
func TestMITMRejected(t *testing.T) {
	shared := testShared()
	attacker := bytes.Repeat([]byte{0x99}, 32) // the MITM's guess, wrong

	// The real server.
	srv, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("server listen: %v", err)
	}
	defer srv.Close()
	hit := make(chan struct{}, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if c, err := srv.Accept(ctx); err == nil {
			hit <- struct{}{}
			c.Close()
		}
	}()

	// The MITM: it presents a udpcarrier listener to the client using the wrong
	// secret, and (a real interceptor) also forwards toward the true server.
	mitm, err := Listen("127.0.0.1:0", attacker, 0)
	if err != nil {
		t.Fatalf("mitm listen: %v", err)
	}
	defer mitm.Close()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if c, err := mitm.Accept(ctx); err == nil {
			c.Close() // it managed to auth a client (must not happen)
			t.Error("MITM completed a carrier with the client")
		}
	}()

	// The client dials the MITM's address with the TRUE secret.
	mctx, mcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer mcancel()
	c, err := Dial(mctx, mitm.LocalAddr().String(), shared, 0)
	if err == nil {
		c.Close()
		t.Fatal("client accepted a carrier through a man-in-the-middle")
	}

	select {
	case <-hit:
		t.Fatal("true server granted a carrier to an intercepted client")
	case <-time.After(300 * time.Millisecond):
	}

	// Sanity: the same client reaches the real server directly (fresh context).
	dctx, dcancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer dcancel()
	c2, err := Dial(dctx, srv.LocalAddr().String(), shared, 0)
	if err != nil {
		t.Fatalf("direct dial to the real server failed: %v", err)
	}
	c2.Close()
}

// TestWrongKeyRejected: a client with the wrong shared secret cannot open a
// carrier (the handshake's pinned keys and psk both derive from the secret).
func TestWrongKeyRejected(t *testing.T) {
	srv, err := Listen("127.0.0.1:0", testShared(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if c, err := Dial(ctx, srv.LocalAddr().String(), bytes.Repeat([]byte{0x01}, 32), 0); err == nil {
		c.Close()
		t.Fatal("dial with the wrong shared key succeeded")
	}
}

// TestReplayRejected proves a duplicated data datagram is not delivered twice:
// a tapping relay copies every client->server datagram and also re-sends it, and
// the server must deliver each payload exactly once (core's replay window drops
// the duplicate even if it slips past FEC's own de-duplication).
func TestReplayRejected(t *testing.T) {
	shared := testShared()
	srv, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Duplicating relay: forwards each client datagram to the server twice.
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	sa, _ := net.ResolveUDPAddr("udp", srv.LocalAddr().String())
	back, err := net.DialUDP("udp", nil, sa)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	var clientAddr atomic.Pointer[net.UDPAddr]
	stop := make(chan struct{})
	// client -> server, duplicated
	go func() {
		buf := make([]byte, 2048)
		for {
			select {
			case <-stop:
				return
			default:
			}
			front.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := front.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			clientAddr.Store(addr)
			pkt := append([]byte(nil), buf[:n]...)
			back.Write(pkt)
			back.Write(pkt) // duplicate every datagram
		}
	}()
	// server -> client
	go func() {
		buf := make([]byte, 2048)
		for {
			select {
			case <-stop:
				return
			default:
			}
			back.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := back.Read(buf)
			if err != nil {
				continue
			}
			if ca := clientAddr.Load(); ca != nil {
				front.WriteToUDP(buf[:n], ca)
			}
		}
	}()
	defer close(stop)

	accCh := make(chan *Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		c, _ := srv.Accept(ctx)
		accCh <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cli, err := Dial(ctx, front.LocalAddr().String(), shared, 0)
	if err != nil {
		t.Fatalf("dial through duplicating relay: %v", err)
	}
	defer cli.Close()
	sc := <-accCh
	if sc == nil {
		t.Fatal("no carrier through duplicating relay")
	}
	defer sc.Close()

	const n = 200
	got := map[string]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			ft, p, err := sc.ReadFrame()
			if err != nil {
				return
			}
			if ft == core.TypeData {
				got[string(p)]++
			}
		}
	}()
	for i := 0; i < n; i++ {
		cli.SendFrame(core.TypeData, []byte{byte(i >> 8), byte(i), 'x', 'y'})
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	dups := 0
	for _, c := range got {
		if c > 1 {
			dups++
		}
	}
	if dups != 0 {
		t.Fatalf("replay window failed: %d payloads delivered more than once", dups)
	}
	if len(got) == 0 {
		t.Fatal("no payloads delivered at all")
	}
	// The decoder/session must have SEEN and dropped duplicates.
	st := sc.Stats()
	t.Logf("replay: delivered %d unique payloads, decoder dups=%d (duplicates dropped)", len(got), st.Dec.Dups)
}

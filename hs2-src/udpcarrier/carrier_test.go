package udpcarrier

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

var debugRate = false

func testShared() []byte { return bytes.Repeat([]byte{0x5a}, 32) }

// pair brings up a listener on loopback and a dialer connected straight to it,
// returning both live carriers. It fails the test if either side does not come
// up within a few seconds.
func pair(t *testing.T) (cli, srv *Conn, l *Listener) {
	t.Helper()
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.LocalAddr().String()

	type acc struct {
		c   *Conn
		err error
	}
	accCh := make(chan acc, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		c, err := l.Accept(ctx)
		accCh <- acc{c, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cli, err = Dial(ctx, addr, shared, 0)
	if err != nil {
		l.Close()
		t.Fatalf("dial: %v", err)
	}
	a := <-accCh
	if a.err != nil {
		cli.Close()
		l.Close()
		t.Fatalf("accept: %v", a.err)
	}
	return cli, a.c, l
}

// readOne reads a single frame with a timeout via a goroutine.
func readOne(t *testing.T, c *Conn, d time.Duration) (byte, []byte) {
	t.Helper()
	type r struct {
		ft byte
		p  []byte
		e  error
	}
	ch := make(chan r, 1)
	go func() {
		ft, p, e := c.ReadFrame()
		ch <- r{ft, p, e}
	}()
	select {
	case got := <-ch:
		if got.e != nil {
			t.Fatalf("ReadFrame: %v", got.e)
		}
		return got.ft, got.p
	case <-time.After(d):
		t.Fatalf("ReadFrame timed out after %s", d)
		return 0, nil
	}
}

func TestRoundTrip(t *testing.T) {
	cli, srv, l := pair(t)
	defer l.Close()
	defer cli.Close()
	defer srv.Close()

	// client -> server
	msg := []byte("hello over udp fec noise")
	if err := cli.SendFrame(core.TypeData, msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	ft, got := readOne(t, srv, 3*time.Second)
	if ft != core.TypeData || !bytes.Equal(got, msg) {
		t.Fatalf("server got ft=%d %q, want %q", ft, got, msg)
	}

	// server -> client
	msg2 := []byte("and back again")
	if err := srv.SendFrame(core.TypeData, msg2); err != nil {
		t.Fatalf("send back: %v", err)
	}
	ft, got = readOne(t, cli, 3*time.Second)
	if ft != core.TypeData || !bytes.Equal(got, msg2) {
		t.Fatalf("client got ft=%d %q, want %q", ft, got, msg2)
	}
}

func TestManyFrames(t *testing.T) {
	cli, srv, l := pair(t)
	defer l.Close()
	defer cli.Close()
	defer srv.Close()

	const n = 500
	got := make(chan []byte, n)
	go func() {
		for i := 0; i < n; i++ {
			ft, p, err := srv.ReadFrame()
			if err != nil {
				return
			}
			if ft == core.TypeData {
				got <- append([]byte(nil), p...)
			}
		}
	}()
	for i := 0; i < n; i++ {
		if err := cli.SendFrame(core.TypeData, []byte(fmt.Sprintf("frame-%04d", i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	deadline := time.After(10 * time.Second)
	seen := 0
	for seen < n {
		select {
		case <-got:
			seen++
		case <-deadline:
			t.Fatalf("only %d/%d frames arrived (loss-free path should deliver all)", seen, n)
		}
	}
}

func TestProbeReachable(t *testing.T) {
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	res, err := Probe(context.Background(), l.LocalAddr().String(), shared, 20, 5*time.Millisecond, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Reachable || res.Received < 18 {
		t.Fatalf("probe on a clean loopback path should see ~all echoes: %+v", res)
	}
	if res.Loss > 0.1 {
		t.Fatalf("unexpected probe loss on loopback: %+v", res)
	}
}

func TestProbeWrongKeyNoReply(t *testing.T) {
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	wrong := bytes.Repeat([]byte{0x11}, 32)
	res, err := Probe(context.Background(), l.LocalAddr().String(), wrong, 15, 5*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Reachable {
		t.Fatalf("probe with the wrong key must get no reply, got %+v", res)
	}
}

package tlscarrier

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// A link that is a pair of connections (CapPair, level 2; decision T16).

// pairServer starts a server whose tunnels go to the returned channel.
func pairServer(t *testing.T, key []byte) (string, chan *Carrier) {
	t.Helper()
	got := make(chan *Carrier, 4)
	addr, stop := startLoggedServer(t, key, &logSink{}, func(c *Carrier) { got <- c })
	t.Cleanup(stop)
	return addr, got
}

// levelCappedServer is a server that speaks levels 1 to levelCap (an older
// one).
func levelCappedServer(t *testing.T, key []byte, levelCap byte) (string, chan *Carrier) {
	t.Helper()
	be, stopB := backend(t)
	got := make(chan *Carrier, 4)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{SharedKey: key, Cert: testCert(t, "vpn.example.com"), BackendAddr: be, levelCap: levelCap}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Handle(ctx, c, func(c *Carrier) { got <- c })
		}
	}()
	t.Cleanup(func() { cancel(); ln.Close(); stopB() })
	return ln.Addr().String(), got
}

func tunnelOf(t *testing.T, got chan *Carrier) *Carrier {
	t.Helper()
	select {
	case c := <-got:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no tunnel")
		return nil
	}
}

// Both connections of a pair are dialled and accepted together: the client
// gets QUIET's carrier with BULK's, the server one tunnel (QUIET's, with
// BULK's) for the two, and each connection carries its own bytes.
func TestPairDialAcceptTogether(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	addr, got := pairServer(t, key)
	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	sc := tunnelOf(t, got)
	if car.Bulk() == nil || sc.Bulk() == nil || car.Level() != LevelPair || car.Caps()&CapPair == 0 {
		t.Fatalf("client pair %v, server pair %v, level %d caps %#x", car.Bulk() != nil, sc.Bulk() != nil, car.Level(), car.Caps())
	}
	select {
	case c := <-got:
		t.Fatalf("a second tunnel (pair %v) for one link", c.Bulk() != nil)
	case <-time.After(200 * time.Millisecond):
	}
	for _, c := range []struct {
		name string
		a, b *Carrier
	}{{"quiet", car, sc}, {"bulk", car.Bulk(), sc.Bulk()}} {
		msg := []byte("hello on " + c.name)
		go c.a.RawConn().Write(msg)
		buf := make([]byte, len(msg))
		c.b.RawConn().SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c.b.RawConn(), buf); err != nil || !bytes.Equal(buf, msg) {
			t.Fatalf("%s: %q, %v", c.name, buf, err)
		}
	}
	if car.DataTCPConn() == nil || car.DataTCPConn() == car.TCPConn() {
		t.Fatal("the data socket is not BULK's")
	}
	// closing the link closes both
	car.Close()
	for _, c := range []*Carrier{sc, sc.Bulk()} {
		c.RawConn().SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.RawConn().Read(make([]byte, 1)); err == nil {
			t.Fatal("a connection of the closed pair still reads")
		}
	}
}

// With a level-1 peer the link falls back to one connection: an older
// server (it speaks level 1 alone) and an older client (no level 2, no
// CapPair, no hello).
func TestPairFallbackLevel1(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	t.Run("old server", func(t *testing.T) {
		addr, got := levelCappedServer(t, key, 1)
		car, err := Dial(addr, "vpn.example.com", key)
		if err != nil {
			t.Fatal(err)
		}
		defer car.Close()
		sc := tunnelOf(t, got)
		if car.Bulk() != nil || sc.Bulk() != nil || car.Level() != 1 || car.Caps()&CapPair != 0 || sc.Caps()&CapPair != 0 {
			t.Fatalf("level %d, client caps %#x pair %v, server caps %#x pair %v", car.Level(), car.Caps(), car.Bulk() != nil, sc.Caps(), sc.Bulk() != nil)
		}
	})
	t.Run("old client", func(t *testing.T) {
		addr, got := pairServer(t, key)
		old := AuthHdr{Ver: protoVersion, MinLevel: 1, MaxLevel: 1, Caps: CapClass | CapSched | CapHostQ | CapSendCap}
		car, err := dialOne(addr, "vpn.example.com", key, "", 2*time.Second, old, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer car.Close()
		sc := tunnelOf(t, got)
		if sc.Bulk() != nil || sc.Level() != 1 || sc.Caps()&CapPair != 0 {
			t.Fatalf("server: level %d caps %#x pair %v", sc.Level(), sc.Caps(), sc.Bulk() != nil)
		}
	})
	t.Run("HS2_TUNE_PAIR=0", func(t *testing.T) {
		t.Setenv("HS2_TUNE_PAIR", "0")
		addr, got := pairServer(t, key)
		car, err := Dial(addr, "vpn.example.com", key)
		if err != nil {
			t.Fatal(err)
		}
		defer car.Close()
		sc := tunnelOf(t, got)
		if car.Bulk() != nil || sc.Bulk() != nil || car.Caps()&CapPair != 0 {
			t.Fatalf("a pair with the switch set: caps %#x", car.Caps())
		}
	})
}

// A BULK half no QUIET half waits for is closed, and makes no tunnel; a
// QUIET half whose BULK half never comes is closed after pairTimeout.
func TestPairHalfAlone(t *testing.T) {
	key := bytes.Repeat([]byte{0x43}, 32)
	addr, got := pairServer(t, key)
	h := LocalAuthHdr()
	stray := &pairHello{role: pairBulk}
	stray.id[0] = 7
	b, err := dialOne(addr, "vpn.example.com", key, "", 2*time.Second, h, stray)
	if err != nil {
		t.Fatal(err)
	}
	b.RawConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.RawConn().Read(make([]byte, 1)); err == nil {
		t.Fatal("a stray BULK half stayed open")
	}
	defer func(d time.Duration) { pairTimeout = d }(pairTimeout)
	pairTimeout = 300 * time.Millisecond
	lone := &pairHello{role: pairQuiet}
	lone.id[0] = 8
	q, err := dialOne(addr, "vpn.example.com", key, "", 2*time.Second, h, lone)
	if err != nil {
		t.Fatal(err)
	}
	q.RawConn().SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := q.RawConn().Read(make([]byte, 1)); err == nil {
		t.Fatal("a QUIET half without its BULK stayed open")
	}
	select {
	case <-got:
		t.Fatal("a half made a tunnel")
	case <-time.After(100 * time.Millisecond):
	}
}

// The hello rides the auth record's pad: the record keeps v2's lengths
// (client 314..754) and its header and tag check as before; the hello reads
// back only with the session's nonce and EKM.
func TestPairHelloInPad(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	ekm := testEKM()
	for i := 0; i < 3000; i++ {
		hello := &pairHello{role: pairQuiet + byte(i%2)}
		hello.id[3] = byte(i)
		rec, nonce := makeClientAuth(key, ekm, LocalAuthHdr(), hello)
		if len(rec) < 314 || len(rec) > 754 {
			t.Fatalf("record length %d with a hello", len(rec))
		}
		n, hdr, more, _, ok := parseClientAuth(key, ekm, rec)
		if !ok || more != 0 || !bytes.Equal(n, nonce) || hdr != LocalAuthHdr() {
			t.Fatalf("record with a hello did not parse: ok %v more %d", ok, more)
		}
		got, ok := parsePairHello(rec[authHeadLen:], key, nonce, ekm)
		if !ok || got != *hello {
			t.Fatalf("hello %+v read back as %+v (%v)", *hello, got, ok)
		}
		if _, ok := parsePairHello(rec[authHeadLen:], key, nonce, testEKMOther()); ok {
			t.Fatal("the hello read with another session's EKM")
		}
		plain, pn := makeClientAuth(key, ekm, LocalAuthHdr(), nil)
		if _, ok := parsePairHello(plain[authHeadLen:], key, pn, ekm); ok {
			t.Fatal("a random pad read as a hello")
		}
	}
}

func testEKMOther() []byte { return bytes.Repeat([]byte{0x5a}, 32) }

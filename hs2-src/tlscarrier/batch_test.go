package tlscarrier

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// countConn records every Write that reaches it and serves reads from src.
type countConn struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
	src    io.Reader
	reads  int
}

func (c *countConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *countConn) Read(p []byte) (int, error) {
	c.reads++
	return c.src.Read(p)
}

// Between Cork and Flush the writes gather and leave as one, byte for byte
// the same; outside a cork every write goes straight through.
func TestBatchConnCorkFlush(t *testing.T) {
	cc := &countConn{}
	b := &batchConn{Conn: cc}
	b.Write([]byte("before"))
	b.Cork()
	var want []byte
	for i := 0; i < 17; i++ {
		p := bytes.Repeat([]byte{byte(i)}, 1000+i)
		want = append(want, p...)
		if n, err := b.Write(p); n != len(p) || err != nil {
			t.Fatalf("corked write: %d %v", n, err)
		}
	}
	if len(cc.writes) != 1 {
		t.Fatalf("%d writes reached the socket while corked, want only the one before", len(cc.writes))
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	b.Write([]byte("after"))
	if len(cc.writes) != 3 || !bytes.Equal(cc.writes[1], want) || string(cc.writes[2]) != "after" {
		t.Fatalf("socket saw %d writes; the gathered one equal: %v", len(cc.writes), len(cc.writes) > 1 && bytes.Equal(cc.writes[1], want))
	}
}

// Reads pass straight through until BufferReads; after it, many small reads
// are served from few socket reads and return exactly the stream.
func TestBatchConnBufferedReads(t *testing.T) {
	stream := make([]byte, 200<<10)
	for i := range stream {
		stream[i] = byte(i * 7)
	}
	cc := &countConn{src: bytes.NewReader(stream)}
	b := &batchConn{Conn: cc}
	small := make([]byte, 5)
	got := make([]byte, 0, len(stream))
	for i := 0; i < 4; i++ {
		n, _ := b.Read(small)
		got = append(got, small[:n]...)
	}
	if cc.reads != 4 {
		t.Fatalf("before BufferReads %d socket reads for 4 reads", cc.reads)
	}
	b.BufferReads()
	before := cc.reads
	buf := make([]byte, 1400)
	for {
		n, err := b.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, stream) {
		t.Fatal("buffered reads changed the stream")
	}
	if r := cc.reads - before; r > len(stream)/batchReadBuf+2 {
		t.Fatalf("%d socket reads for %d KiB, want about %d", r, len(stream)>>10, len(stream)/batchReadBuf)
	}
}

// With HS2_SHAPE_FAST a real link has a batchConn under TLS on both ends,
// found through the TLS conn, and carries data as before.
func TestBatchedLinkEndToEnd(t *testing.T) {
	old := Batch
	Batch = true
	defer func() { Batch = old }()
	key := bytes.Repeat([]byte{0x22}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	got := make(chan *Carrier, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { got <- c })
	defer stop()
	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	var srv *Carrier
	select {
	case srv = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no tunnel")
	}
	defer srv.Close()
	cb, sb := BatcherOf(car.RawConn()), BatcherOf(srv.RawConn())
	if cb == nil || sb == nil {
		t.Fatalf("batcher under TLS: client %v server %v", cb != nil, sb != nil)
	}
	if car.TCPConn() == nil || srv.TCPConn() == nil {
		t.Fatal("the TCP socket is no longer found under the batchConn")
	}
	cb.BufferReads()
	sb.BufferReads()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	go func() {
		cb.Cork()
		for p := payload; len(p) > 0; {
			n := min(1000, len(p))
			car.RawConn().Write(p[:n]) // one TLS record each, gathered by the cork
			p = p[n:]
		}
		cb.Flush()
	}()
	recv := make([]byte, len(payload))
	srv.RawConn().SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(srv.RawConn(), recv); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recv, payload) {
		t.Fatal("payload changed through a batched link")
	}
}

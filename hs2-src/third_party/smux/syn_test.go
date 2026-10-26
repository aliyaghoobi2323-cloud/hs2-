package smux

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// synPair is a client and a server session over a pipe, protocol version 2,
// without keepalives.
func synPair(t *testing.T) (client, server *Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	cfg := DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	client, err := Client(c1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err = Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func acceptWithin(t *testing.T, s *Session, d time.Duration) (*Stream, error) {
	t.Helper()
	s.SetDeadline(time.Now().Add(d))
	defer s.SetDeadline(time.Time{})
	return s.AcceptStream()
}

// The wire layout of the payload (hs2 wire v3, §4.2): user tag BE, the class
// hint in bits 0-1 of the next byte (role 0), group id 0, seq base 0.
func TestEncodeSyn(t *testing.T) {
	want := [SynPayloadLen]byte{0xA1, 0xB2, 0xC3, 0xD4, 2}
	if p := EncodeSyn(0xA1B2C3D4, 2); p != want {
		t.Fatalf("EncodeSyn = %x, want %x", p, want)
	}
	if p := EncodeSyn(1, 0xFF); p[4] != 3 {
		t.Fatalf("class byte %#x: a hint is two bits, the role stays 0", p[4])
	}
}

// What the opener puts in the SYN is what the acceptor reads from Syn(); a
// plain OpenStream (the control streams) sends tag 0, class 0.
func TestSynPayloadDelivered(t *testing.T) {
	client, server := synPair(t)
	st, err := client.OpenStreamSyn(EncodeSyn(0xA1B2C3D4, 1))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := acceptWithin(t, server, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tag, hint := acc.Syn(); tag != 0xA1B2C3D4 || hint != 1 {
		t.Fatalf("acceptor read tag %#x hint %d, want 0xA1B2C3D4 1", tag, hint)
	}
	if tag, hint := st.Syn(); tag != 0xA1B2C3D4 || hint != 1 {
		t.Fatalf("opener has tag %#x hint %d", tag, hint)
	}
	if _, err := client.OpenStream(); err != nil {
		t.Fatal(err)
	}
	acc, err = acceptWithin(t, server, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tag, hint := acc.Syn(); tag != 0 || hint != 0 {
		t.Fatalf("plain open: tag %#x hint %d, want 0 0", tag, hint)
	}
	if n := server.BadSyn(); n != 0 {
		t.Fatalf("BadSyn = %d, want 0", n)
	}
}

// rawFrame is one frame as a raw peer reads it.
type rawFrame struct {
	cmd  byte
	sid  uint32
	data []byte
}

// rawFrames reads frames off conn until it fails.
func rawFrames(conn io.Reader) <-chan rawFrame {
	out := make(chan rawFrame, 64)
	go func() {
		defer close(out)
		var hdr rawHeader
		for {
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return
			}
			data := make([]byte, hdr.Length())
			if _, err := io.ReadFull(conn, data); err != nil {
				return
			}
			out <- rawFrame{hdr.Cmd(), hdr.StreamID(), data}
		}
	}()
	return out
}

func writeRaw(t *testing.T, conn io.Writer, cmd byte, sid uint32, data []byte) {
	t.Helper()
	b := make([]byte, headerSize+len(data))
	b[0], b[1] = 2, cmd
	binary.LittleEndian.PutUint16(b[2:], uint16(len(data)))
	binary.LittleEndian.PutUint32(b[4:], sid)
	copy(b[headerSize:], data)
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
}

// nextFrame returns the next frame that is not a window update.
func nextFrame(t *testing.T, frames <-chan rawFrame) rawFrame {
	t.Helper()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("the session closed the connection")
			}
			if f.cmd != cmdUPD {
				return f
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no frame")
		}
	}
}

// A SYN without the payload, or with one of another length, opens no stream:
// the opener gets a FIN for it and the session counts it, and the session
// goes on serving good SYNs.
func TestBadSynRefused(t *testing.T) {
	c1, c2 := net.Pipe()
	cfg := DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	server, err := Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	frames := rawFrames(c1)
	writeRaw(t, c1, cmdSYN, 3, nil)
	writeRaw(t, c1, cmdSYN, 5, make([]byte, 12))
	for _, sid := range []uint32{3, 5} {
		if f := nextFrame(t, frames); f.cmd != cmdFIN || f.sid != sid {
			t.Fatalf("got cmd %d sid %d, want a FIN for %d", f.cmd, f.sid, sid)
		}
	}
	good := EncodeSyn(7, 0)
	writeRaw(t, c1, cmdSYN, 7, good[:])
	st, err := acceptWithin(t, server, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID() != 7 {
		t.Fatalf("accepted stream %d: a refused SYN reached AcceptStream", st.ID())
	}
	if n := server.BadSyn(); n != 2 {
		t.Fatalf("BadSyn = %d, want 2", n)
	}
	if n := server.NumStreams(); n != 1 {
		t.Fatalf("%d streams, want only the good one", n)
	}
}

// A sub-stream role needs a group (§4.2): role ≠ 0 with group id 0 is refused
// like a bad length.
func TestSynRoleWithoutGroupRefused(t *testing.T) {
	c1, c2 := net.Pipe()
	cfg := DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	server, err := Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	frames := rawFrames(c1)
	p := EncodeSyn(9, 1)
	p[4] |= 1 << 2 // role 1, group id 0
	writeRaw(t, c1, cmdSYN, 3, p[:])
	if f := nextFrame(t, frames); f.cmd != cmdFIN || f.sid != 3 {
		t.Fatalf("got cmd %d sid %d, want a FIN for 3", f.cmd, f.sid)
	}
	if _, err := acceptWithin(t, server, 100*time.Millisecond); err == nil {
		t.Fatal("a SYN with a role and no group reached AcceptStream")
	}
	if n := server.BadSyn(); n != 1 {
		t.Fatalf("BadSyn = %d, want 1", n)
	}
}

// What the opener writes at once goes out after its SYN, and the acceptor has
// the payload when it accepts the stream.
func TestSynPayloadBeforeData(t *testing.T) {
	c1, c2 := net.Pipe()
	cfg := DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	client, err := Client(c1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	frames := rawFrames(c2)
	data := []byte("hello right after the open")
	syn := EncodeSyn(0x01020304, 2)
	done := make(chan error, 1)
	go func() {
		st, err := client.OpenStreamSyn(syn)
		if err == nil {
			_, err = st.Write(data)
		}
		done <- err
	}()
	f := nextFrame(t, frames)
	if f.cmd != cmdSYN || !bytes.Equal(f.data, syn[:]) {
		t.Fatalf("first frame: cmd %d data %x, want the SYN with %x", f.cmd, f.data, syn)
	}
	g := nextFrame(t, frames)
	if g.cmd != cmdPSH || g.sid != f.sid || !bytes.Equal(g.data, data) {
		t.Fatalf("second frame: cmd %d sid %d data %q", g.cmd, g.sid, g.data)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// End to end: the acceptor reads the payload, then the data, intact.
	client2, server := synPair(t)
	go func() {
		st, err := client2.OpenStreamSyn(syn)
		if err == nil {
			st.Write(data)
		}
	}()
	acc, err := acceptWithin(t, server, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tag, hint := acc.Syn(); tag != 0x01020304 || hint != 2 {
		t.Fatalf("tag %#x hint %d", tag, hint)
	}
	acc.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(data))
	if _, err := io.ReadFull(acc, got); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q (%v)", got, err)
	}
}

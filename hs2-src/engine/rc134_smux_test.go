package engine

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// rc134GateConn: Write blocks while the test holds mu (a socket not draining).
type rc134GateConn struct {
	net.Conn
	mu sync.Mutex
}

func (g *rc134GateConn) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Conn.Write(b)
}

// hs2 rc134: a Write that gives up (deadline) on a frame sendLoop has not
// taken yet must not have that frame's buffer read later. Upstream let
// sendLoop copy frame.data after Write returned; hs2 then reused the buffer
// (pooled chunk) and the frame went out with another record's bytes. The
// frame still goes out (control.go counts on it), with the bytes B had when
// Write returned: the peer must read exactly A, B, C (sha256), never B's
// scribbled bytes.
func TestSmuxWriteGiveUpNeverSendsReusedBuffer(t *testing.T) {
	for _, ver := range []int{1, 2} {
		for run := 0; run < 5; run++ {
			a, b := net.Pipe()
			g := &rc134GateConn{Conn: a}
			cfg := smux.DefaultConfig()
			cfg.Version = ver
			cfg.KeepAliveInterval = time.Hour
			cfg.KeepAliveTimeout = 2 * time.Hour
			cli, err := smux.Client(g, cfg)
			if err != nil {
				t.Fatal(err)
			}
			srv, err := smux.Server(b, cfg)
			if err != nil {
				t.Fatal(err)
			}
			st, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Write([]byte{1}); err != nil { // the peer accepts
				t.Fatal(err)
			}
			ps, err := srv.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			one := make([]byte, 1)
			if _, err := io.ReadFull(ps, one); err != nil {
				t.Fatal(err)
			}
			fill := func(n int, v byte) []byte {
				p := make([]byte, n)
				for i := range p {
					p[i] = v + byte(i%7)
				}
				return p
			}
			A, B, C := fill(1000, 0x10), fill(1000, 0x40), fill(1000, 0x70)
			g.mu.Lock() // the socket stops draining
			aDone := make(chan error, 1)
			go func() { _, err := st.Write(A); aDone <- err }()
			time.Sleep(20 * time.Millisecond) // sendLoop takes A and blocks
			st.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := st.Write(B); err != smux.ErrTimeout {
				g.mu.Unlock()
				t.Fatalf("v%d run %d: B: %v, want timeout", ver, run, err)
			}
			B0 := append([]byte(nil), B...)
			for i := range B {
				B[i] = 0xEE // the caller reuses its buffer (hs2: putChunk)
			}
			st.SetWriteDeadline(time.Time{})
			g.mu.Unlock()
			if err := <-aDone; err != nil {
				t.Fatal(err)
			}
			if _, err := st.Write(C); err != nil {
				t.Fatal(err)
			}
			want := append(append(append([]byte{}, A...), B0...), C...)
			got := make([]byte, len(want))
			ps.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(ps, got); err != nil {
				t.Fatalf("v%d run %d: read: %v", ver, run, err)
			}
			if sha256.Sum256(got) != sha256.Sum256(want) {
				t.Fatalf("v%d run %d: corrupted stream (0xEE bytes: %d)", ver, run, bytes.Count(got, []byte{0xEE}))
			}
			ps.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if n, _ := ps.Read(one); n > 0 {
				t.Fatalf("v%d run %d: more bytes than written", ver, run)
			}
			cli.Close()
			srv.Close()
		}
	}
}

// TestMConnRawSwitchChecksumStress moved to raw137_test.go (with the raw137 fix).

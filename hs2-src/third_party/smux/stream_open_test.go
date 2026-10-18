package smux

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// synConn holds back the return of the session's SYN write until the
// session's reader has taken in want bytes and come back for more: what a
// busy machine may do to OpenStream between its SYN going out and
// writeFrame returning, while the peer accepts the stream and sends on it.
type synConn struct {
	net.Conn
	want   int64
	read   int64 // atomic
	inRead int32 // atomic
}

func (c *synConn) Read(b []byte) (int, error) {
	atomic.StoreInt32(&c.inRead, 1)
	n, err := c.Conn.Read(b)
	atomic.StoreInt32(&c.inRead, 0)
	atomic.AddInt64(&c.read, int64(n))
	return n, err
}

func (c *synConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if len(b) == headerSize && b[1] == cmdSYN {
		for t := time.Now(); time.Since(t) < 5*time.Second; time.Sleep(time.Millisecond) {
			if atomic.LoadInt64(&c.read) >= c.want && atomic.LoadInt32(&c.inRead) == 1 {
				break
			}
		}
	}
	return n, err
}

// What the peer sends on a stream as soon as it has accepted it -- data, its
// FIN -- reaches the stream even when it comes before OpenStream returns.
// (recvLoop dropped it: a download lost its first frames and ended short, or
// stalled from the start waiting for a window update for data never read.)
func TestOpenStreamGetsWhatPeerSendsAtOnce(t *testing.T) {
	for _, ver := range []int{1, 2} {
		want := bytes.Repeat([]byte{'x'}, 100)
		c1, c2 := net.Pipe()
		conn := &synConn{Conn: c1, want: int64(headerSize + len(want) + headerSize)}
		config := DefaultConfig()
		config.Version = ver
		config.KeepAliveDisabled = true
		client, err := Client(conn, config)
		if err != nil {
			t.Fatal(err)
		}
		server, err := Server(c2, config)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			st, err := server.AcceptStream()
			if err != nil {
				return
			}
			st.Write(want)
			st.Close()
		}()
		st, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		st.SetReadDeadline(time.Now().Add(time.Second))
		got, err := io.ReadAll(st)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("version %d: read %d bytes (%v), want the %d the peer sent and its FIN", ver, len(got), err, len(want))
		}
		client.Close()
		server.Close()
	}
}

package smux

import (
	"io"
	"net"
	"testing"
	"time"
)

// hs2 er94: a Write already blocked (peer window full, or the socket not
// draining: a blackholed link) wakes with ErrTimeout when SetWriteDeadline
// moves the deadline into the past. Upstream read the deadline once per
// Write, so the RESCUE of a raw sub stuck on a dead link never got the
// writer off it.
func TestWriteCutWakesBlockedWrite(t *testing.T) {
	for _, mode := range []string{"window", "socket"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			cfg := DefaultConfig()
			cfg.Version = 2
			cfg.KeepAliveInterval = time.Hour
			cfg.KeepAliveTimeout = 2 * time.Hour
			cli, err := Client(a, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			if mode == "window" {
				srv, err := Server(b, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer srv.Close()
				go func() { // accepts, never reads: the peer window fills
					st, err := srv.AcceptStream()
					if err == nil {
						<-st.GetDieCh()
					}
				}()
			} else {
				go func() { // the SYN only, then the socket stops draining
					var h [headerSize]byte
					io.ReadFull(b, h[:])
				}()
			}
			st, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := st.Write(make([]byte, 8<<20))
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("write returned early: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			st.SetWriteDeadline(time.Unix(1, 0))
			select {
			case err := <-done:
				if err != ErrTimeout {
					t.Fatalf("err %v, want ErrTimeout", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("a blocked Write did not wake on SetWriteDeadline(past)")
			}
		})
	}
}

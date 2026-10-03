package engine

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// serveReverseLink must report WHY a reverse link ended, over the real TLS +
// smux stack, so the exit pool's log can tell the edge closing a link (clean
// TLS close: the autopilot shrinking the pattern) from the network killing it
// (an RST, as `ss -K` or a middlebox produces). Field report: both were logged
// as "retired — pattern shrinking".
func TestServeReverseLinkReportsWhyItEnded(t *testing.T) {
	cases := []struct {
		name     string
		end      func(edge *tlscarrier.Carrier)
		want     string
		edgeShut bool
		slow     bool
	}{
		{"edge closes the link (clean TLS close)", func(edge *tlscarrier.Carrier) { edge.Close() },
			"read: " + reasonPeerClosed, true, false},
		{"network resets the link (RST)", func(edge *tlscarrier.Carrier) {
			tc := edge.TCPConn()
			if tc == nil {
				t.Fatal("no TCP conn under the edge carrier")
			}
			tc.SetLinger(0) // close with RST instead of FIN, and without a TLS close_notify
			tc.Close()
		}, "read: reset by the network or the other server", false, false},
		// A bare FIN (e.g. the edge process crashed) is indistinguishable from
		// close_notify to crypto/tls — both are a clean close, and the log
		// says only that ("closed by the edge"), no more.
		{"edge sends a bare FIN (no close_notify)", func(edge *tlscarrier.Carrier) { edge.TCPConn().CloseWrite() },
			"read: " + reasonPeerClosed, true, false},
		// The edge stays connected but sends nothing (a stalled path): smux's
		// keepalive gives up and closes the link locally. That must read as a
		// stall, not as "closed locally".
		{"path stalls (edge silent): keepalive timeout", func(*tlscarrier.Carrier) {},
			"no data from the edge for 24s (keepalive timeout — path stalled)", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.slow && testing.Short() {
				t.Skip("waits for the 24s keepalive timeout")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			key := bytes.Repeat([]byte{0x5a}, 32)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
			edgeCar := make(chan *tlscarrier.Carrier, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) { edgeCar <- car })
			}()

			// The exit dials (reverse) and serves the link until it ends.
			exitCar, err := tlscarrier.DialFrom(ln.Addr().String(), "lab.example.com", key, "")
			if err != nil {
				t.Fatal(err)
			}
			why := make(chan string, 1)
			go func() { why <- serveReverseLink(ctx, exitCar, KharejConfig{}, nil, nil) }()

			var edge *tlscarrier.Carrier
			select {
			case edge = <-edgeCar:
			case <-time.After(3 * time.Second):
				t.Fatal("the edge never got the link")
			}
			time.Sleep(100 * time.Millisecond) // the exit is now blocked reading the link
			c.end(edge)

			select {
			case got := <-why:
				if got != c.want {
					t.Fatalf("reason = %q, want %q", got, c.want)
				}
				if edgeClosed(got) != c.edgeShut {
					t.Fatalf("edgeClosed(%q) = %v, want %v", got, edgeClosed(got), c.edgeShut)
				}
			case <-time.After(40 * time.Second):
				t.Fatal("serveReverseLink did not return after the link ended")
			}
		})
	}
}

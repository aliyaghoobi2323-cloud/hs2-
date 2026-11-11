package tlscarrier

import (
	"context"
	"net"
	"testing"
	"time"
)

// rel47: a reality link's carriers reach the kernel socket on BOTH sides, as
// tls links do (server_probe_test.go). The engine reads TCP_INFO through
// Carrier.TCPConn for the call's late-copy test (srtt), the call pick
// (busy), the autopilot (retransmits) and the wedge watch; a reality link
// that hid its socket would be blind to all of them on a path that degrades.
func TestRealityCarrierTCPConn(t *testing.T) {
	for _, kind := range []string{"reality", FrontBoth} {
		t.Run(kind, func(t *testing.T) {
			rt := newRawTarget(t, "TARGET", 0)
			priv, pub, err := RealityKeygen()
			if err != nil {
				t.Fatal(err)
			}
			lp := &FrontProfile{Kind: kind, Target: rt.ln.Addr().String(), ServerNames: []string{"www.target.test"},
				PrivateKey: priv, ShortIDs: []string{"a1b2c3d4"}}
			key := []byte("0123456789abcdef0123456789abcdef")
			srv, err := NewRealityServer(lp, &Server{SharedKey: key})
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			got := make(chan *Carrier, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				srv.AcceptFront(context.Background(), c, func(car *Carrier) { got <- car })
			}()
			dp := &FrontProfile{Kind: "reality", ServerNames: []string{"www.target.test"}, PublicKey: pub, ShortIDs: []string{"a1b2c3d4"}}
			car, err := DialPathFront(context.Background(), DialPath{Addr: ln.Addr().String(), Key: key, Profile: dp, Connect: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer car.Close()
			if car.TCPConn() == nil {
				t.Fatal("dialer's reality carrier: TCPConn() is nil (TCP_INFO dead)")
			}
			select {
			case s := <-got:
				defer s.Close()
				if s.TCPConn() == nil {
					t.Fatal("listener's reality carrier: TCPConn() is nil (TCP_INFO dead)")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no server carrier")
			}
		})
	}
}

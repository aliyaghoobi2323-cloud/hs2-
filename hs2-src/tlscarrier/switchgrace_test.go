package tlscarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// holdTarget: a TLS target for www.target.test that reads and never answers
// (like openssl s_server -www waiting for a whole HTTP request).
func holdTarget(t *testing.T) string {
	cert := testCert(t, "www.target.test")
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tln.Close() })
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					c.SetReadDeadline(time.Now().Add(20 * time.Second))
					if _, err := c.Read(b); err != nil {
						return
					}
				}
			}()
		}
	}()
	return tln.Addr().String()
}

// rel45: a live both -> reality switch. Inside the grace a tls dial with our
// name is served (not spliced to a target that never answers it); after it,
// the tls leg of "both" sees the target's certificate and fails at once
// (ErrTargetAnswered) instead of waiting out the 10 s auth deadline.
func TestRealitySwitchGrace(t *testing.T) {
	l := newRealityLab(t, holdTarget(t), nil) // pure reality
	be, stop := backend(t)
	t.Cleanup(stop)
	grace := 1500 * time.Millisecond
	l.setup.Lock()
	l.srv.TLS = &Server{SharedKey: l.key, Cert: testCert(t, "tun.ours.test"), BackendAddr: be}
	l.srv.TLSName = func(_ net.Addr, sni string) bool { return sni == "tun.ours.test" }
	l.srv.TLSUntil = time.Now().Add(grace)
	l.setup.Unlock()
	end := time.Now().Add(grace)

	tlsPath := DialPath{Addr: l.addr, SNI: "tun.ours.test", Key: l.key, Connect: 2 * time.Second}
	tc, err := DialPathFront(context.Background(), tlsPath)
	if err != nil {
		t.Fatalf("tls inside the grace: %v", err)
	}
	echoOK(t, tc)
	tc.Close()
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatalf("reality inside the grace: %v", err)
	}
	echoOK(t, car)
	car.Close()
	if s := l.srv.Counters.Snap(); s.TLS != 1 || s.Reality != 1 {
		t.Fatalf("counters inside the grace: %+v", s)
	}

	time.Sleep(time.Until(end) + 100*time.Millisecond)
	bp := *l.dprof
	bp.Kind = FrontBoth
	leg := tlsPath
	leg.Profile = &bp
	t0 := time.Now()
	_, err = TLSFront{}.DialFront(context.Background(), leg)
	took := time.Since(t0)
	if !errors.Is(err, ErrTargetAnswered) {
		t.Fatalf("tls leg after the grace: err %v (want ErrTargetAnswered)", err)
	}
	if took > 2*time.Second {
		t.Fatalf("tls leg after the grace failed after %v (want at the handshake)", took)
	}
	if s := l.srv.Counters.Snap(); s.TLS != 1 {
		t.Fatalf("a tls dial after the grace reached tls: %+v", s)
	}
	// reality keeps working after the grace
	car, err = l.dial(l.dprof)
	if err != nil {
		t.Fatalf("reality after the grace: %v", err)
	}
	car.Close()
}

// A plain tls path (no reality profile) never checks the certificate's name.
func TestTargetCertCheck(t *testing.T) {
	ours := testCert(t, "tun.ours.test").Certificate
	tgt := testCert(t, "www.target.test").Certificate
	if err := targetCertCheck("tun.ours.test", []string{"www.target.test"})(tgt, nil); !errors.Is(err, ErrTargetAnswered) {
		t.Fatalf("target cert: %v", err)
	}
	if err := targetCertCheck("tun.ours.test", []string{"www.target.test"})(ours, nil); err != nil {
		t.Fatalf("our cert: %v", err)
	}
	// our SNI is the target's name too (a tls front that borrows the name): never refused
	if err := targetCertCheck("www.target.test", []string{"www.target.test"})(tgt, nil); err != nil {
		t.Fatalf("same name: %v", err)
	}
	if err := targetCertCheck("tun.ours.test", []string{"other.test"})(tgt, nil); err != nil {
		t.Fatalf("unrelated cert: %v", err)
	}
}

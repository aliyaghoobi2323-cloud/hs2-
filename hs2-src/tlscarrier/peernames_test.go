package tlscarrier

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// rel47: a PLAIN tls path (no reality profile of its own: a cold dialer, or
// one whose remembered fronts predate the IP's switch) to a pure reality IP
// is spliced to the target. With the peer's reality names known — read when
// the certificate arrives, so an announcement that lands while the dial is in
// flight counts — the dial fails at the handshake with ErrTargetAnswered
// instead of waiting 10 s for a proof (lab: ~25 start links stuck 10 s, the
// pool half built, the call probe dead). Unknown names: the old behaviour; a
// tls IP of the same peer (our cert) is never refused.
func TestPlainTLSPeerTargetNames(t *testing.T) {
	l := newRealityLab(t, holdTarget(t), nil) // pure reality, target www.target.test
	var names atomic.Pointer[[]string]
	peer := func() []string {
		if p := names.Load(); p != nil {
			return *p
		}
		return nil
	}
	known := []string{"www.target.test"}
	names.Store(&known)
	plain := DialPath{Addr: l.addr, SNI: "tun.ours.test", Key: l.key, Connect: 2 * time.Second, PeerTargetNames: peer}
	t0 := time.Now()
	_, err := DialPathFront(context.Background(), plain)
	if !errors.Is(err, ErrTargetAnswered) {
		t.Fatalf("plain tls to a pure reality IP, names known: %v (want ErrTargetAnswered)", err)
	}
	if took := time.Since(t0); took > 2*time.Second {
		t.Fatalf("failed after %v, want at the handshake", took)
	}

	// names unknown (nothing announced or remembered): no check, the dial
	// waits for the proof as before (bounded here by a short context-free
	// check: the handshake itself succeeds, so no ErrTargetAnswered)
	names.Store(&[]string{})
	done := make(chan error, 1)
	go func() { _, err := DialPathFront(context.Background(), plain); done <- err }()
	select {
	case err := <-done:
		if errors.Is(err, ErrTargetAnswered) {
			t.Fatal("refused with no names known")
		}
	case <-time.After(1500 * time.Millisecond): // waiting for the proof: the old path
	}

	// a tls IP of the same peer (our own cert for our SNI) is never refused
	be, stop := backend(t)
	t.Cleanup(stop)
	srv := &Server{SharedKey: l.key, Cert: testCert(t, "tun.ours.test"), BackendAddr: be}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Handle(context.Background(), c, func(car *Carrier) {
				go func() {
					buf := make([]byte, 64)
					n, _ := car.RawConn().Read(buf)
					car.RawConn().Write(buf[:n])
					car.Close()
				}()
			})
		}
	}()
	names.Store(&known)
	tp := plain
	tp.Addr = ln.Addr().String()
	car, err := DialPathFront(context.Background(), tp)
	if err != nil {
		t.Fatalf("plain tls to our own tls IP with names known: %v", err)
	}
	car.Close()
}

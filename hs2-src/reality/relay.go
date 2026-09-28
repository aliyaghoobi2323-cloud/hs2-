package reality

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// Path 1: full certificate relay, the way real Reality works.
//
// Instead of presenting a self-signed cert for the cover domain (which a probe
// that validates certs would catch instantly), the server FORWARDS the client's
// ClientHello to the genuine target site and relays the target's real handshake
// messages — real ServerHello, real certificate, real CertificateVerify — back
// to whoever connected. A probe therefore completes a genuine TLS handshake
// against the real target's real certificate. Nothing to catch.
//
// The only thing that distinguishes an authorised client from a probe is the
// hidden signal in the ClientHello's session-id (verified earlier by the
// Dispatcher). For a probe, we relay the WHOLE session transparently, so it
// talks to the real site end to end. For an authorised client, we relay the
// handshake so the client sees the real cert too, then after the handshake the
// server switches the connection to the hs2 tunnel.
//
// SECURITY-CRITICAL SUBTLETY (this is where Reality itself had bugs): the relay
// must be a faithful byte pipe during the handshake. Any message the real
// target sends that we fail to relay — including post-handshake NewSessionTicket
// records — becomes a distinguisher. So for probes we never stop relaying; we
// pipe until the connection closes.

// relayProbe pipes an unauthorised connection to the real target, byte for
// byte, forever. alreadyRead is the ClientHello we consumed; it is sent to the
// target first so the target sees a complete handshake.
func relayProbe(ctx context.Context, client net.Conn, alreadyRead []byte, targetAddr string) {
	defer client.Close()
	target, err := net.DialTimeout("tcp", targetAddr, 8*time.Second)
	if err != nil {
		return
	}
	defer target.Close()
	if len(alreadyRead) > 0 {
		if _, err := target.Write(alreadyRead); err != nil {
			return
		}
	}
	pipe(ctx, client, target)
}

// pipe copies in both directions until either side closes. This is the faithful
// relay: every byte the target sends (including NewSessionTicket) reaches the
// client, so the client cannot tell it is not talking straight to the target.
func pipe(ctx context.Context, a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		// Half-close so the peer sees EOF, like a real relay.
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

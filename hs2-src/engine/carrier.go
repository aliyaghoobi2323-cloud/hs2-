package engine

import "context"

// Carrier is the seam between the engine and any transport. The engine owns the
// persistent TUN, failover and multipath; a Carrier owns ONE secure link and
// knows how to turn payloads into framed, encrypted bytes and back. The engine
// never learns whether the bytes are protected by Noise (udp/tcp carriers) or
// by TLS (the reality carrier) — it just sends and receives frames.
//
// This is what lets the reality carrier (single TLS layer, no Noise) and the
// Noise carriers coexist without the engine encrypting twice: each carrier
// brings its OWN encryption, and the engine adds none.
type Carrier interface {
	// SendFrame delivers one payload of the given type to the peer, encrypted
	// and framed by the carrier. It must be safe to call from one goroutine at
	// a time (the engine serialises sends).
	SendFrame(ftype byte, payload []byte) error

	// ReadFrame blocks for the next frame from the peer, decrypted and
	// unframed. A returned error means the link is dead and the engine should
	// tear this carrier down and reconnect.
	ReadFrame() (ftype byte, payload []byte, err error)

	// Close terminates the underlying connection.
	Close() error
}

// CarrierDialer establishes a Carrier as the initiator (the side that dials).
// Reconnection calls this again; each call is a fresh secure link.
type CarrierDialer interface {
	Dial(ctx context.Context) (Carrier, error)
}

// CarrierListener accepts incoming Carriers as the responder. Accept blocks
// until a peer completes the carrier's handshake; carriers that fail
// authentication (e.g. a probe on the reality carrier) are handled inside the
// listener and never surface here.
type CarrierListener interface {
	Accept(ctx context.Context) (Carrier, error)
	Close() error
}

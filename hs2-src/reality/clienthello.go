package reality

import "errors"

// Minimal TLS ClientHello parser: just enough to pull the client_random and the
// session_id out of the first handshake record, without a full TLS stack. We
// only need those two fields to verify the signal; everything else about the
// ClientHello is genuine (produced by uTLS) and is forwarded untouched if the
// signal does not verify.
//
// Record layer:   [type(1)=22][ver(2)][len(2)][handshake...]
// Handshake:      [type(1)=1 ClientHello][len(3)][ver(2)][random(32)]
//                 [sid_len(1)][session_id(sid_len)]...

var errNotClientHello = errors.New("reality: not a TLS ClientHello")

type clientHello struct {
	clientRandom []byte // 32 bytes
	sessionID    []byte // 0..32 bytes
	raw          []byte // the full bytes read, for forwarding
}

func parseClientHello(b []byte) (*clientHello, error) {
	if len(b) < 5 || b[0] != 0x16 { // TLS handshake record
		return nil, errNotClientHello
	}
	recLen := int(b[3])<<8 | int(b[4])
	if len(b) < 5+recLen {
		return nil, errNotClientHello // incomplete; caller forwards raw
	}
	h := b[5 : 5+recLen]
	if len(h) < 4 || h[0] != 0x01 { // ClientHello
		return nil, errNotClientHello
	}
	hsLen := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	body := h[4:]
	if len(body) < hsLen || hsLen < 2+32+1 {
		return nil, errNotClientHello
	}
	p := body
	p = p[2:] // legacy_version
	cr := p[:32]
	p = p[32:]
	sidLenByte := int(p[0])
	p = p[1:]
	if sidLenByte > len(p) {
		return nil, errNotClientHello
	}
	sid := p[:sidLenByte]
	return &clientHello{clientRandom: cr, sessionID: sid, raw: b}, nil
}

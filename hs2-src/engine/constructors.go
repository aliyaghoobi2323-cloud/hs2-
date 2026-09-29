package engine

import (
	"crypto/tls"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Exported constructors so cmd can build carriers without touching internals.

func NewRealityDialer(addr, sni string, sharedKey []byte) CarrierDialer {
	return &realityDialer{addr: addr, sni: sni, sharedKey: sharedKey}
}

func NewRealityListener(addr, coverAddr string, sharedKey []byte, cert tls.Certificate) (CarrierListener, error) {
	return newRealityListener(addr, coverAddr, sharedKey, cert)
}

// bindIP (optional) is the local source IP the carrier dials from.
func NewNoiseDialer(addr, bindIP string, local core.StaticKey, remoteStatic, psk []byte) CarrierDialer {
	return &noiseDialer{addr: addr, bindIP: bindIP, local: local, remoteStatic: remoteStatic, psk: psk}
}

func NewNoiseListener(addr string, local core.StaticKey, psk []byte) (CarrierListener, error) {
	return newNoiseListener(addr, local, psk)
}

// UDP carrier (Noise + adaptive FEC over a datagram socket). Keys are derived
// from the shared secret inside udpcarrier, so only the shared key is needed.

func NewUDPDialer(addr, bindIP string, shared []byte, mtu int) CarrierDialer {
	return &udpDialer{addr: addr, bindIP: bindIP, shared: shared, mtu: mtu}
}

func NewUDPListener(addr string, shared []byte, mtu int) (CarrierListener, error) {
	return newUDPListener(addr, shared, mtu)
}

// Auto transport: probe UDP, use it when it is reachable with FEC-recoverable
// loss, otherwise fall back to the TCP (noise) carrier — silently, without
// dropping the engine's persistent TUN.

func NewAutoDialer(addr, bindIP string, shared []byte, mtu int, logf func(string, ...any)) CarrierDialer {
	return &autoDialer{addr: addr, bindIP: bindIP, shared: shared, mtu: mtu, log: logf}
}

func NewAutoListener(addr string, shared []byte, mtu int) (CarrierListener, error) {
	return newAutoListener(addr, shared, mtu)
}

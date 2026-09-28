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

func NewNoiseDialer(addr string, local core.StaticKey, remoteStatic, psk []byte) CarrierDialer {
	return &noiseDialer{addr: addr, local: local, remoteStatic: remoteStatic, psk: psk}
}

func NewNoiseListener(addr string, local core.StaticKey, psk []byte) (CarrierListener, error) {
	return newNoiseListener(addr, local, psk)
}

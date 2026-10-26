package engine

import "net/netip"

// userTagV6Prefix is the IPv6 prefix that is one user (§3.1): a subscriber
// gets a /64, and its privacy addresses rotate inside it.
const userTagV6Prefix = 64

// UserTag is the 32-bit tag of the user at ip (§3.1): the FNV-1a hash
// (flowHash's) of an IPv4 address, or of the first v6Prefix bits of an IPv6
// one. It means something only inside the tunnel, so it needs no key. The
// side that opens a stream computes it — the edge, from the client's address
// — and the SYN carries it; the other side reads it from there.
func UserTag(ip netip.Addr, v6Prefix int) uint32 {
	if !ip.IsValid() {
		return 0
	}
	if ip = ip.Unmap(); ip.Is4() {
		a := ip.As4()
		return fnvAdd(fnvOffset, a[:])
	}
	v6Prefix = min(max(v6Prefix, 0), 128)
	a := ip.As16()
	full := v6Prefix / 8
	h := fnvAdd(fnvOffset, a[:full])
	if rem := v6Prefix % 8; rem != 0 {
		h = fnvAdd(h, []byte{a[full] & byte(0xFF<<(8-rem))})
	}
	return h
}

package engine

import (
	"cmp"
	"fmt"
	"net/netip"
)

// qKey (m4f251, audit246 M4) is one shaper filter: the packets this tunnel's
// links send to a peer. A link this side DIALED is keyed by the peer's
// address and port (ip dst + dport), as before. A link this side ACCEPTED
// (the exit in direct mode, the Iran edge in reverse mode) is keyed by the
// peer's address and OUR listen port (ip dst + TCP sport): every accepted
// link to that peer shares it, so 600 links from one or a few edge addresses
// are 1-16 filters instead of 600 (one u32 key per link, walked linearly by
// every egress packet of the host while a cap holds). The keys no longer
// change with link churn (the peer's ephemeral ports are not in them), so the
// hl234 generation swap only runs on a real change (a new peer address).
//
// Selection stays exact: a TCP packet from our listen port can only belong to
// a socket of our listener (a bound listening port is never handed out as an
// ephemeral one, and another process cannot listen on it), and only those to
// this peer address are matched; other services on the same IP (xray, a
// second tunnel) use other ports.
type qKey struct {
	ip    netip.Addr
	port  uint16
	sport bool // port is our own (listen) port: match the TCP source port
}

// qDst is the key of a dialed link: the peer's address and port.
func qDst(ap netip.AddrPort) qKey { return qKey{ip: ap.Addr().Unmap(), port: ap.Port()} }

// qDsts is qDst for each (tests, callers with dialed peers only).
func qDsts(aps ...netip.AddrPort) []qKey {
	k := make([]qKey, 0, len(aps))
	for _, a := range aps {
		k = append(k, qDst(a))
	}
	return k
}

// qLinkKey is a link's key: accepted links by (peer IP, local port).
func qLinkKey(local, remote netip.AddrPort, accepted bool) qKey {
	if accepted && local.Port() != 0 {
		return qKey{ip: remote.Addr().Unmap(), port: local.Port(), sport: true}
	}
	return qDst(remote)
}

func (k qKey) compare(o qKey) int {
	if c := k.ip.Compare(o.ip); c != 0 {
		return c
	}
	if c := cmp.Compare(k.port, o.port); c != 0 {
		return c
	}
	switch {
	case k.sport == o.sport:
		return 0
	case !k.sport:
		return -1
	}
	return 1
}

func (k qKey) String() string {
	if k.sport {
		return fmt.Sprintf("%s<-:%d", k.ip, k.port)
	}
	return netip.AddrPortFrom(k.ip, k.port).String()
}

// match is the key's u32 selector. sport keys also match the protocol (TCP):
// a UDP socket may hold the same port number.
func (k qKey) match() string {
	if k.ip.Is6() {
		if k.sport {
			return fmt.Sprintf("match ip6 dst %s/128 match ip6 protocol 6 0xff match ip6 sport %d 0xffff", k.ip, k.port)
		}
		return fmt.Sprintf("match ip6 dst %s/128 match ip6 dport %d 0xffff", k.ip, k.port)
	}
	if k.sport {
		return fmt.Sprintf("match ip dst %s/32 match ip protocol 6 0xff match ip sport %d 0xffff", k.ip, k.port)
	}
	return fmt.Sprintf("match ip dst %s/32 match ip dport %d 0xffff", k.ip, k.port)
}

// qKeySet collects one tick's link keys, each once (first-seen order).
type qKeySet struct {
	seen map[qKey]bool
	keys []qKey
}

func (s *qKeySet) add(local, remote netip.AddrPort, accepted bool) {
	if !remote.IsValid() {
		return
	}
	k := qLinkKey(local, remote, accepted)
	if s.seen == nil {
		s.seen = map[qKey]bool{}
	}
	if !s.seen[k] {
		s.seen[k] = true
		s.keys = append(s.keys, k)
	}
}

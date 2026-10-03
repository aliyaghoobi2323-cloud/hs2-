package engine

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-port routing: which panel inbound each Iran user port reaches.
//
// The Iran server (edge) opens the user ports; the kharej server (exit) has the
// panel. Before this, every user port reached the ONE address the exit had
// (expose). Now the edge says, on every user connection, which of its user
// ports the user came in on — a port NUMBER only — and the exit looks that
// number up in its OWN table:
//
//	port_map entry for the port   -> that target (a bare "P" means 127.0.0.1:P)
//	else expose (the default)     -> the default panel, exactly as before
//	else                          -> refused (logged)
//
// The table lives on the exit on purpose. The edge never names an address, so
// a compromised Iran server cannot make the kharej server dial anything the
// kharej operator did not list (127.0.0.1:22, a database, the panel's admin
// port, …). An exit without a port_map, and an older edge that does not say
// the port, behave exactly as before: everything reaches expose.

// RouteTable is the exit's routing table.
type RouteTable struct {
	Default string         // expose: where a port with no entry goes ("" = refuse)
	Ports   map[int]string // user port -> target host:port
}

// Target returns where a connection that came in on the edge's user port goes.
// port 0 means the edge did not say (an older hs2): the default.
func (t RouteTable) Target(port int) (string, bool) {
	if port > 0 {
		if a, ok := t.Ports[port]; ok {
			return a, true
		}
	}
	return t.Default, t.Default != ""
}

// MappedPorts returns the ports with their own entry, ascending.
func (t RouteTable) MappedPorts() []int {
	out := make([]int, 0, len(t.Ports))
	for p := range t.Ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// sameHostPort is the target a bare "P" entry means: the same port on this
// server's loopback.
func sameHostPort(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// ParsePortMap reads a port_map value: comma-separated entries "P" (the same
// port on 127.0.0.1) or "P=host:port". It returns the table and, for a bad
// entry, an error naming it. A port listed twice is an error too: the second
// would silently win.
func ParsePortMap(s string) (map[int]string, error) {
	out := map[int]string{}
	for _, raw := range strings.Split(s, ",") {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		ps, target, hasTarget := strings.Cut(e, "=")
		ps, target = strings.TrimSpace(ps), strings.TrimSpace(target)
		p, err := strconv.Atoi(ps)
		if err != nil || p < 1 || p > 65535 || ps != strconv.Itoa(p) {
			return nil, fmt.Errorf("%q: %q is not a port (1-65535)", e, ps)
		}
		if !hasTarget {
			target = sameHostPort(p)
		} else if err := checkTarget(target); err != nil {
			return nil, fmt.Errorf("%q: %v", e, err)
		}
		if _, dup := out[p]; dup {
			return nil, fmt.Errorf("port %d is listed twice", p)
		}
		out[p] = target
	}
	return out, nil
}

// checkTarget validates a target "host:port".
func checkTarget(t string) error {
	host, port, err := net.SplitHostPort(t)
	if err != nil || host == "" {
		return fmt.Errorf("target %q must be host:port (e.g. 127.0.0.1:2053)", t)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("target %q: %q is not a port (1-65535)", t, port)
	}
	return nil
}

// FormatPortMap writes a table back in port_map form, ascending, with the
// short "P" form for an entry that is the same port on 127.0.0.1.
func FormatPortMap(m map[int]string) string {
	t := RouteTable{Ports: m}
	var b strings.Builder
	for i, p := range t.MappedPorts() {
		if i > 0 {
			b.WriteByte(',')
		}
		if m[p] == sameHostPort(p) {
			b.WriteString(strconv.Itoa(p))
		} else {
			fmt.Fprintf(&b, "%d=%s", p, m[p])
		}
	}
	return b.String()
}

// noRouteLog says, at most once a minute per port, that a user port reached
// this server with nowhere to go — the operator's cue that the kharej side
// needs a port_map entry (or an expose) for it.
type noRouteLog struct {
	mu   sync.Mutex
	last map[string]time.Time // by "port/proto"
}

const noRouteEvery = time.Minute

func (n *noRouteLog) note(port int, proto string, logf func(string, ...any)) {
	if n == nil || logf == nil {
		return
	}
	n.mu.Lock()
	now := time.Now()
	if n.last == nil {
		n.last = map[string]time.Time{}
	}
	key := strconv.Itoa(port) + "/" + proto
	due := now.Sub(n.last[key]) >= noRouteEvery
	if due {
		n.last[key] = now
	}
	n.mu.Unlock()
	if !due {
		return
	}
	if port == 0 {
		logf("ports: a %s connection that does not say its user port (an older Iran server, or one that has not learned this server's table yet) has no target: this server has no expose (default panel) — refused", proto)
		return
	}
	logf("ports: Iran user port %d (%s) has no target on this server — no port_map entry for it and no expose (default panel) — refused. Add it here: Tunnel manager → Ports", port, proto)
}

package engine

import "sync/atomic"

// Kernel TCP memory pressure. When a server's TCP buffers pass the kernel's
// pressure mark (tcp_mem[1]) every TCP socket on it is squeezed: receive
// windows shrink and arriving segments are pruned, so every link resends and
// waits — none of it the links' fault. On a 2 GB server twenty downloads
// whose app stopped reading (each holding its socket buffers full, 16 MB at
// most on that profile) took it there: the guard released none of them (no
// link's own buffer was full — one stalled download per link), and the
// health rules drained 11 healthy links for loss and as stuck, cutting their
// other users (real servers, 250 users: 44 cut, p50 84 → 170 ms).
//
// So while this server — or, on the edge, the other one (its stats records
// carry statsFlagMemPressure) — is under pressure:
//   - the guard resets the connections whose app has taken nothing for
//     stuckFor even when their link's buffer is not full (wedge.go): they are
//     what holds the memory;
//   - no link is judged for loss or as stuck, and not for the recovery
//     window after (linkmanager.go, sampleHealth).
var (
	tcpMemPressure  atomic.Bool // this server, set by the daemon's status writer
	peerMemPressure atomic.Bool // the other server, from its links' stats records (edge)
)

// SetTCPMemPressure says whether this server's kernel TCP memory is above its
// pressure mark (the daemon checks /proc/net/sockstat every status tick).
func SetTCPMemPressure(on bool) { tcpMemPressure.Store(on) }

// TCPMemPressure reports this server's pressure as last set.
func TCPMemPressure() bool { return tcpMemPressure.Load() }

// memPressure reports whether either server is under kernel TCP memory
// pressure, as last seen.
func memPressure() bool { return tcpMemPressure.Load() || peerMemPressure.Load() }

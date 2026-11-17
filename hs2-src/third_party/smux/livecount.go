package smux

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"
)

// hs2 mem66: process-wide levels for leak diagnosis — the streams held in
// every session's map (opened or accepted, not yet closed locally and not
// dropped with their session) and the sessions not yet closed.
var liveStreams, liveSessions atomic.Int64

// LiveStreams returns the streams held in all sessions' maps.
func LiveStreams() int64 { return liveStreams.Load() }

// LiveSessions returns the sessions not yet closed.
func LiveSessions() int64 { return liveSessions.Load() }

// CountObjects (hs2 mem66, HS2_MEM_OBJS=1): also count the Stream and Session
// objects not yet collected (runtime.AddCleanup), open or not — the gap to
// LiveStreams is closed streams something still references.
var CountObjects atomic.Bool

var objStreams, objSessions atomic.Int64

// TrackObject counts p in c until it is collected (CountObjects only).
func TrackObject[T any](p *T, c *atomic.Int64) {
	if !CountObjects.Load() {
		return
	}
	c.Add(1)
	runtime.AddCleanup(p, func(c *atomic.Int64) { c.Add(-1) }, c)
}

// Objects returns the Stream and Session objects not yet collected.
func Objects() (streams, sessions int64) { return objStreams.Load(), objSessions.Load() }

// hs2 mem69 (HS2_MEM_OBJS diagnosis): every session held weakly, for the
// heap-dump retainer search (DeadSessions).
var sessReg struct {
	mu sync.Mutex
	m  map[weak.Pointer[Session]]struct{}
}

func regSession(s *Session) {
	if !CountObjects.Load() {
		return
	}
	sessReg.mu.Lock()
	if sessReg.m == nil {
		sessReg.m = map[weak.Pointer[Session]]struct{}{}
	}
	sessReg.m[weak.Make(s)] = struct{}{}
	sessReg.mu.Unlock()
}

// SessionAddrs returns the addresses of the sessions not yet collected,
// closed ones and open ones apart (diagnosis only).
func SessionAddrs() (closed, open []uintptr) {
	sessReg.mu.Lock()
	defer sessReg.mu.Unlock()
	for w := range sessReg.m {
		s := w.Value()
		if s == nil {
			delete(sessReg.m, w)
			continue
		}
		if s.IsClosed() {
			closed = append(closed, uintptr(unsafe.Pointer(s)))
		} else {
			open = append(open, uintptr(unsafe.Pointer(s)))
		}
	}
	return
}

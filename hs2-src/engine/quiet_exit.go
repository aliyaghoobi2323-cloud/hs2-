package engine

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/xtaci/smux"
)

// The exit's end of β2's group connections (quiet.go): a user connection
// whose main stream names a group is served as that group (exitGroup); the
// move stream the edge opens on a BULK link joins it (exitGroupSet.attach),
// and the exit's next frame goes there too. The link a connection is on
// now is the one its relay's wedge guard watches.

// exitGroup is a group connection at the exit, with the meter of the link
// it is on now.
type exitGroup struct {
	*smux.Group
	mtr atomic.Pointer[linkMeter]
}

func (g *exitGroup) guard() *sessGuard {
	if m := g.mtr.Load(); m != nil {
		return m.guard
	}
	return nil
}

// exitGroupSet is the exit's live group connections, by id (one edge's
// ids: they are random and the edge never reuses a live one, §4.2).
type exitGroupSet struct {
	mu sync.Mutex
	m  map[uint32]*exitGroup
	// tallies: joined by their move stream, move streams refused (no live
	// group), main streams refused (an id already live)
	attached, refused, collided atomic.Int64
}

var exitGroups exitGroupSet

// add serves st, a main stream that names a group, as that group (nil: not
// a group's, or its id is live already: refused, §4.5).
func (s *exitGroupSet) add(st *smux.Stream, mtr *linkMeter) *exitGroup {
	g := smux.AcceptGroup(st)
	if g == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[uint32]*exitGroup{}
	}
	if _, live := s.m[g.ID()]; live {
		s.collided.Add(1)
		return nil
	}
	eg := &exitGroup{Group: g}
	eg.mtr.Store(mtr)
	s.m[g.ID()] = eg
	return eg
}

// remove forgets eg once its connection ended.
func (s *exitGroupSet) remove(eg *exitGroup) {
	s.mu.Lock()
	if s.m[eg.ID()] == eg {
		delete(s.m, eg.ID())
	}
	s.mu.Unlock()
}

// attach joins st, a move stream, to its live group (false: none, or it
// moved already: the caller refuses st).
func (s *exitGroupSet) attach(st *smux.Stream, mtr *linkMeter) bool {
	id, _, _ := st.GroupInfo()
	s.mu.Lock()
	eg := s.m[id]
	s.mu.Unlock()
	if eg == nil || eg.Attach(st) != nil {
		s.refused.Add(1)
		return false
	}
	eg.mtr.Store(mtr)
	s.attached.Add(1)
	return true
}

// line is the exit's group status line.
func (s *exitGroupSet) line() string {
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	return fmt.Sprintf("groups: %d open; moved to bulk %d, move streams refused %d, ids refused %d",
		n, s.attached.Load(), s.refused.Load(), s.collided.Load())
}

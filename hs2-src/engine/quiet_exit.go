package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
	// big: its writer's time inside big writes (the move hint, hint.go)
	big bigWrites
}

// Write is the group's, with its big writes timed (the move hint).
func (g *exitGroup) Write(p []byte) (int, error) { return g.big.write(p, g.Group.Write) }

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

// live is the group connections open now.
func (s *exitGroupSet) live() []*exitGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*exitGroup, 0, len(s.m))
	for _, eg := range s.m {
		out = append(out, eg)
	}
	return out
}

// remove forgets eg once its connection ended.
func (s *exitGroupSet) remove(eg *exitGroup) {
	s.mu.Lock()
	if s.m[eg.ID()] == eg {
		delete(s.m, eg.ID())
	}
	s.mu.Unlock()
}

// attachWait: a move stream that finds no live group waits this long for
// its main stream, which comes on another link (§4.5: a sub-stream whose
// group is not made within 2 s is refused). Refused at once, a move stream
// that beat its main stream (held by an RTO on the QUIET link, or by the
// exit's reader of that link parked on a full buffer) left the connection
// wedged: the exit read the main stream up to the move and waited for the
// rest for good (the R1 red team).
var attachWait = 2 * time.Second

// attach joins st, a move stream, to its live group (false: none within
// attachWait, or it moved already: the caller refuses st).
func (s *exitGroupSet) attach(st *smux.Stream, mtr *linkMeter) bool {
	id, _, _ := st.GroupInfo()
	until := time.Now().Add(attachWait)
	var eg *exitGroup
	for {
		s.mu.Lock()
		eg = s.m[id]
		s.mu.Unlock()
		if eg != nil || !time.Now().Before(until) {
			break
		}
		select {
		case <-st.GetDieCh():
			s.refused.Add(1)
			return false // its link died meanwhile
		case <-time.After(10 * time.Millisecond):
		}
	}
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

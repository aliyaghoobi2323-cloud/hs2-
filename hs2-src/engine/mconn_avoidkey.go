package engine

import "sync/atomic"

// mem70: an avoid list must not keep a link alive.
//
// The Iran heap dump after the mem69 fixes: ~100 of 104 dead smux sessions
// were held by goroutines waiting in (*mconnEdge).open (up to mconnFirstWait
// per try, repeated while the pool had nothing that fit) whose avoid list
// held the *mtcpLink itself — from deadAvoidLocked and raw1.key — and the
// link its whole stack. An avoid entry for an *mtcpLink is now its linkAvoidID,
// a plain number: the picker compares by ID (avoided) and a waiting open
// holds nothing of the link. Other Link kinds (tests) stay themselves.

// linkAvoidID is an avoid entry for an *mtcpLink (avoidKey).
type linkAvoidID uint64

var linkAvoidSeq atomic.Uint64

// avoidID is l's avoid identity, assigned on first use (links are built as
// literals in many places, tests included).
func (l *mtcpLink) avoidID() linkAvoidID {
	if v := l.aid.Load(); v != 0 {
		return linkAvoidID(v)
	}
	n := linkAvoidSeq.Add(1)
	if l.aid.CompareAndSwap(0, n) {
		return linkAvoidID(n)
	}
	return linkAvoidID(l.aid.Load())
}

// avoidKey is the avoid-list form of a sub's key (or a link): an *mtcpLink
// becomes its ID, anything else stays itself.
func avoidKey(k any) any {
	if l, ok := k.(*mtcpLink); ok && l != nil {
		return l.avoidID()
	}
	return k
}

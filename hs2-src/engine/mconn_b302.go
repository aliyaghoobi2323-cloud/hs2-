package engine

import "sync/atomic"

// b302 (2x scale, Iran): HS2_B302_COPYCNT (default on; =0: rel69m's walk).
// The call/lite placement predicates (pickPlaneCall's light test, the
// call-rescue and rescue-join picks) asked linkCopies(link) for every link,
// under LinkManager.mu, and linkCopies walked the link's whole flows map
// (taking flowMu) to count the copyOnly streams: O(all open streams) per
// pick. At 2x (crowd 6000) that walk was ~5.4 s CPU per 20 s inside the
// lock, and the lock (held ~14 s per 20 s by pickPlaneCall) made up ~9.5k s
// of ~11.8k s Iran mutex wait.
//
// With the key on, each mtcpLink keeps copies = the number of streams in
// flows whose copyOnly is set, kept under flowMu by the only two places that
// change it: markCopy (sets copyOnly; counts it only while the stream is in
// flows) and countedStream.Close (removes it from flows; uncounts it if it
// was a copy). linkCopies then reads one atomic: the same number the walk
// would count at that instant.
var b302CopyCnt atomic.Bool

func init() { b302CopyCnt.Store(envBoolDefault("HS2_B302_COPYCNT", true)) }

// markCopy sets c.copyOnly and keeps the link's copy count.
func (c *countedStream) markCopy() {
	l := c.link
	if l == nil {
		c.copyOnly.Store(true)
		return
	}
	l.flowMu.Lock()
	if !c.copyOnly.Load() {
		c.copyOnly.Store(true)
		if _, in := l.flows[c]; in {
			l.copies.Add(1)
		}
	}
	l.flowMu.Unlock()
}

// linkCopiesWalk is rel69m's linkCopies: count the copyOnly streams in flows.
func linkCopiesWalk(ml *mtcpLink) int {
	n := 0
	ml.flowMu.Lock()
	for cs := range ml.flows {
		if cs.copyOnly.Load() {
			n++
		}
	}
	ml.flowMu.Unlock()
	return n
}

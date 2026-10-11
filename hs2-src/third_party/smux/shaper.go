package smux

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// shaperHeap orders the frames waiting for the session's writer: control
// frames first, in the order they were queued; then data frames by their
// fair-queue tags (fairQueue): the one that starts first, then the one that
// ends first (the shorter), then the one queued first.
type shaperHeap []writeRequest

func (h shaperHeap) Len() int { return len(h) }
func (h shaperHeap) Less(i, j int) bool {
	if h[i].class != h[j].class {
		return h[i].class < h[j].class
	}
	if h[i].start != h[j].start {
		return h[i].start < h[j].start
	}
	if h[i].class == CLSDATA && len(h[i].frame.data) != len(h[j].frame.data) {
		return len(h[i].frame.data) < len(h[j].frame.data)
	}
	return _itimediff(h[j].seq, h[i].seq) > 0
}

func (h shaperHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *shaperHeap) Push(x interface{}) { *h = append(*h, x.(writeRequest)) }

func (h *shaperHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// fairQueue shares the session's writer among the streams with data to send
// by bytes, start-time fair queuing: each data frame is tagged with the
// virtual time its stream's data reaches when the frame starts (start: where
// the stream's previous frame finished, or now -- vt -- if the stream has
// fallen behind or was idle) and when it ends (finish: start plus the
// frame's bytes), and the writer takes the frame that starts first, the one
// that ends first among those that start together.
//
// A stream's writer waits for each frame before it queues the next, so with
// queuing by arrival alone, as before, the streams took turns frame by frame:
// a frame of a stream that sends little (a request, a reply, hs2's control
// ping) waited behind one frame of every stream with data queued, a round of
// 16 KiB frames. Now a stream that was idle or below its share starts at vt,
// ahead of every frame of a stream that has been sending at its share or
// more (those start at vt or later), and a small frame ends before the large
// ones that start with it: it goes next, behind only the frame being written.
// The streams that keep a frame queued take turns by bytes; none can starve
// another, and one stream's frames leave in the order they were queued (its
// tags only grow).
//
// Only the session's shaperLoop uses it.
type fairQueue struct {
	vt     uint64            // start tag of the last data frame handed to the writer
	finish map[uint32]uint64 // per stream, the finish tag of its last data frame
	sweep  int               // size of finish at which entries at or behind vt are dropped
}

// tag gives the data frame of r its start tag; its finish tag is the start
// plus its bytes (shaperHeap reads that from the frame).
func (q *fairQueue) tag(r *writeRequest) {
	if q.finish == nil {
		q.finish = make(map[uint32]uint64)
	}
	sid := r.frame.sid
	r.start = q.vt
	if f := q.finish[sid]; f > r.start {
		r.start = f
	}
	q.finish[sid] = r.start + uint64(headerSize+len(r.frame.data))
	if len(q.finish) > q.sweep {
		// a stream whose finish tag vt has reached starts at vt anyway:
		// its entry is dropped (streams closed long ago, idle ones)
		for id, f := range q.finish {
			if f <= q.vt {
				delete(q.finish, id)
			}
		}
		q.sweep = 2*len(q.finish) + 64
	}
}

// sent moves the virtual time to the start of the data frame r, just handed
// to the writer: every frame still queued starts there or later.
func (q *fairQueue) sent(r *writeRequest) {
	if r.class == CLSDATA && r.start > q.vt {
		q.vt = r.start
	}
}

package core

// replayWindow is a sliding bitmap that rejects duplicated or too-old sequence
// numbers. It is the datagram defence a stream carrier gets for free from TCP:
// on UDP a captured frame can be re-sent, and without this it would decrypt and
// be delivered a second time.
//
// The window tracks the highest sequence seen and a bitmap of the WINDOW
// numbers below it. A sequence above the top slides the window up; one inside
// is accepted once and then marked; one below the floor is refused outright.
// This is the WireGuard/IPsec construction (RFC 6479-style), sized modestly.
//
// The bitmap is addressed by (seq mod replayBits) in 64-bit words, so sliding
// up by one only clears the single bit that enters the window. The first
// version shifted a 2048-entry array on every packet, which cost a 2 KiB
// memmove per packet — invisible on a stream carrier that calls it a few
// thousand times a second, but the UDP carrier runs it on every datagram.

const (
	replayBits  = 2048
	replayWords = replayBits / 64
)

type replayWindow struct {
	top    uint64 // highest accepted sequence
	bm     [replayWords]uint64
	primed bool
}

func (w *replayWindow) bit(seq uint64) (word uint64, mask uint64) {
	i := seq % replayBits
	return i / 64, 1 << (i % 64)
}

func (w *replayWindow) get(seq uint64) bool {
	i, m := w.bit(seq)
	return w.bm[i]&m != 0
}

func (w *replayWindow) set(seq uint64) {
	i, m := w.bit(seq)
	w.bm[i] |= m
}

func (w *replayWindow) clear(seq uint64) {
	i, m := w.bit(seq)
	w.bm[i] &^= m
}

// check reports whether seq is fresh, and records it if so. A false return
// means the frame must be dropped: it is a replay, or older than the window.
func (w *replayWindow) check(seq uint64) bool {
	if !w.primed {
		w.primed = true
		w.top = seq
		w.set(seq)
		return true
	}
	if seq > w.top {
		// The sequences between the old top and the new one have not been
		// seen: clear their bits so a later arrival is still accepted once.
		if seq-w.top >= replayBits {
			for i := range w.bm {
				w.bm[i] = 0
			}
		} else {
			for s := w.top + 1; s <= seq; s++ {
				w.clear(s)
			}
		}
		w.top = seq
		w.set(seq)
		return true
	}
	// seq <= top
	if w.top-seq >= replayBits {
		return false // too old
	}
	if w.get(seq) {
		return false // already seen
	}
	w.set(seq)
	return true
}

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

const replayBits = 2048

type replayWindow struct {
	top    uint64           // highest accepted sequence
	bitmap [replayBits]bool // bitmap[i] => (top - i) has been seen
	primed bool
}

// check reports whether seq is fresh, and records it if so. A false return
// means the frame must be dropped: it is a replay, or older than the window.
func (w *replayWindow) check(seq uint64) bool {
	if !w.primed {
		w.primed = true
		w.top = seq
		w.bitmap[0] = true
		return true
	}
	if seq > w.top {
		// Slide up by (seq - top): clear the bits that fall off the new bottom.
		shift := seq - w.top
		if shift >= replayBits {
			for i := range w.bitmap {
				w.bitmap[i] = false
			}
		} else {
			// Move existing marks down by shift.
			for i := replayBits - 1; i >= 0; i-- {
				var src bool
				if uint64(i) >= shift {
					src = w.bitmap[uint64(i)-shift]
				}
				w.bitmap[i] = src
			}
			for i := uint64(0); i < shift; i++ {
				w.bitmap[i] = false
			}
		}
		w.top = seq
		w.bitmap[0] = true
		return true
	}
	// seq <= top
	diff := w.top - seq
	if diff >= replayBits {
		return false // too old
	}
	if w.bitmap[diff] {
		return false // already seen
	}
	w.bitmap[diff] = true
	return true
}

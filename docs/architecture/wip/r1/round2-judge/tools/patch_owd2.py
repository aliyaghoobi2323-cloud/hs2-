#!/usr/bin/env python3
"""patch_owd2.py HS2SRC: fix 2, second version: the one-way delay per BULK
link (owd.go v2) in place of the QUIET link's (v1)."""
import sys

root = sys.argv[1]


def patch(rel, pairs):
    p = root + "/" + rel
    s = open(p, encoding="utf-8").read()
    for old, new in pairs:
        if s.count(old) != 1:
            sys.exit("%s: %d matches for %r" % (rel, s.count(old), old[:80]))
        s = s.replace(old, new)
    open(p, "w", encoding="utf-8", newline="\n").write(s)


patch("engine/stream_kharej.go", [
    ("""	case kindOWD:
		owdLoop(ctx, st) // α's one-way delays on a QUIET link (owd.go)
""", """	case kindOWD:
		if mtr == nil {
			st.Close()
			break
		}
		owdLoop(ctx, st, &mtr.owd) // α's one-way delays on a BULK link (owd.go)
"""),
])

patch("engine/stream_iran.go", [
    ("""		go openOWD(ctx, l) // a QUIET link: α's one-way delays (owd.go)
""", """		go openOWD(ctx, l) // a BULK link: α's one-way delays (owd.go)
"""),
])

patch("engine/sendcap.go", [
    ("""	owdNoteRate(long)
""", ""),
    ("""	// OWD: the queue delay of this end's sending direction from the QUIET
	// link's one-way delays (owd.go; OWDok false: none), α's in place of
	// SRTT - MinRTT""", """	// OWD: the queue delay of this end's sending direction from the link's
	// one-way delays (owd.go; OWDok false: none), α's in place of
	// SRTT - MinRTT"""),
    ("""	blocked   func() int64
	blockSeen int64
}""", """	blocked   func() int64
	blockSeen int64
	// owd: the link's one-way delays (a BULK link's, owd.go; nil: none),
	// and the bytes received at the reading before, for its beat
	owd  *owdLink
	rx0  uint64
	rxAt time.Duration
}"""),
    ("""		var oq time.Duration
		ook := false
		if l.core.alpha {
			oq, ook = owdQueue()
		}""", """		var oq time.Duration
		ook := false
		if l.owd != nil {
			// the beat follows the busier direction of the link
			rx := 0.0
			if l.rxAt > 0 && now > l.rxAt && ts.bytesReceived >= l.rx0 {
				rx = float64(ts.bytesReceived-l.rx0) / (now - l.rxAt).Seconds()
			}
			l.rx0, l.rxAt = ts.bytesReceived, now
			l.owd.noteRate(max(l.core.maxDlv(), rx))
			if l.core.alpha {
				oq, ook = l.owd.queue()
			}
		}"""),
])

patch("engine/stream.go", [
    ("""			scl.core.alpha = bulkLink && !AlphaOff()
""", """			scl.core.alpha = bulkLink && !AlphaOff()
			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
			}
"""),
])

patch("engine/health.go", [
    ("""type linkMeter struct {
""", """type linkMeter struct {
	// owd: the link's one-way queue delays (a BULK link's, owd.go)
	owd owdLink
"""),
])
print("patched")

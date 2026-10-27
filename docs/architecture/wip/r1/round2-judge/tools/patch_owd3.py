#!/usr/bin/env python3
"""patch_owd3.py HS2SRC: fix 2, third version: a message's stamp is when it
leaves the host (its send time plus the socket's unsent bytes at the pacing
rate), and α keeps its start while the link's one-way delay is not heard
yet (no srtt fallback then)."""
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


patch("engine/owd.go", [
    ("""//   - each end stamps a message with its monotonic clock (ctrlNow) every
//     owdTickOf(rate) while the link is busy; the receiver keeps the least""",
     """//   - each end stamps a message with its monotonic clock (ctrlNow) every
//     owdTickOf(rate) while the link is busy, moved on by the time the
//     socket's unsent bytes take at its pacing rate (owdLocalWait): the
//     stamp is when the message leaves the host, so the wait behind this
//     end's own unsent data is no queue (version 2 counted it: 20 KiB
//     unsent is 18 ms at a 9 Mbit/s cap, so a low cap read a queue, cut
//     and read a longer one; a download beside an upload took 20 s to
//     reach 80 Mbit/s on the rig); the receiver keeps the least"""),
    ("""	rate, rateAt atomic.Int64 // float64 bits, bytes/s; ctrlNow
	msgs         atomic.Int64
}""", """	rate, rateAt atomic.Int64 // float64 bits, bytes/s; ctrlNow
	msgs         atomic.Int64
	// sys: the link socket's TCP_INFO (set before the loop starts; nil:
	// the stamp is the send time)
	sys capSys
}

// owdLocalWait is how long (ns) a byte written now waits in the socket
// before it is sent: the unsent bytes at the pacing rate (or the delivery
// rate without one; 0 when neither is known).
func owdLocalWait(ts tcpStat) int64 {
	r := float64(ts.pacingRate)
	if r <= 0 || r > 1e12 {
		r = float64(ts.deliveryRate)
	}
	if r <= 0 || ts.notsent == 0 {
		return 0
	}
	return int64(float64(ts.notsent) / r * 1e9)
}"""),
    ("""		owdEncode(b[:], ctrlNow(), o.qIn.Load(), fresh)""", """		stamp := ctrlNow()
		if o.sys != nil {
			if ts, ok := o.sys.info(); ok {
				stamp += owdLocalWait(ts)
			}
		}
		owdEncode(b[:], stamp, o.qIn.Load(), fresh)"""),
])

patch("engine/stream.go", [
    ("""			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
			}""", """			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
				meter.owd.sys = sys
			}"""),
])

patch("engine/sendcap.go", [
    ("""	OWD   time.Duration
	OWDok bool
}""", """	OWD   time.Duration
	OWDok bool
	// OWDwait: the link has one-way delays (OWD on) but none heard yet:
	// α's start holds (srtt − min_rtt holds the other direction's queue)
	OWDwait bool
}"""),
    ("""			if l.core.alpha {
				oq, ook = l.owd.queue()
			}
		}""", """			if l.core.alpha {
				oq, ook = l.owd.queue()
			}
		}
		owait := l.owd != nil && l.core.alpha && !ook && !OWDOff()"""),
    ("""			MinRTT: usDur(uint64(ts.minRTT)), Received: ts.bytesReceived, OWD: oq, OWDok: ook}))""",
     """			MinRTT: usDur(uint64(ts.minRTT)), Received: ts.bytesReceived, OWD: oq, OWDok: ook, OWDwait: owait}))"""),
    ("""	if c.aStart && (c.aOWD || s.SRTT > 0 && s.MinRTT > 0) {
		if c.aQ <= alphaLow {""", """	if c.aStart && (c.aOWD || s.OWDwait || s.SRTT > 0 && s.MinRTT > 0) {
		if c.aQ <= alphaLow || s.OWDwait {"""),
])
print("patched")

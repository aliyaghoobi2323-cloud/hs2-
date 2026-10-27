#!/usr/bin/env python3
"""patch_alpha.py HS2SRC: the judge's two fixes to β2's α (sendcap.go), the
OWD hooks (stream_iran.go, stream_kharej.go, stream.go) and the switches
(cmd/hs2/main.go). Exact replacements: fails loudly if β2's text moved."""
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


patch("engine/sendcap.go", [
    ("""	MinRTT         time.Duration // the socket's min_rtt (0: unknown): α's queue delay, SRTT - MinRTT
	Received       uint64        // bytes received on the socket (tcpi_bytes_received), cumulative: α's other direction
}""", """	MinRTT         time.Duration // the socket's min_rtt (0: unknown): α's queue delay, SRTT - MinRTT
	Received       uint64        // bytes received on the socket (tcpi_bytes_received), cumulative: α's other direction
	// OWD: the queue delay of this end's sending direction from the QUIET
	// link's one-way delays (owd.go; OWDok false: none), α's in place of
	// SRTT - MinRTT
	OWD   time.Duration
	OWDok bool
}"""),
    ("""	aSec  int64
	// tallies (the status line)""", """	aSec  int64
	// aStart: no queue of its own seen since α armed (the judge's fix 1:
	// the cap is C4-lite's till then); aOWD: the latest q is the one-way
	// delay (fix 2), not srtt − min_rtt
	aStart, aOWD bool
	// tallies (the status line)"""),
    ("""// TuneAlpha 0 (HS2_TUNE_ALPHA=0, lab): BULK links keep C4-lite's cap.
var TuneAlpha = 1
""", """// TuneAlpha 0 (HS2_TUNE_ALPHA=0, lab): BULK links keep C4-lite's cap.
var TuneAlpha = 1

// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab): α starts from the
// delivery rate it armed at and grows a few % a reading, as β2 had it.
//
// The judge's fix 1 (R1 round 2): β2's α armed at the first delivery
// sample of a BULK socket that flows had just moved to (4 to 10 Mbit/s) and
// grew by alphaGrowFast a reading, 1.34 times a second: a 100 Mbit/s path
// took 7 s to fill (one_vs_16 on the rig: 10, 14, 18, 25, 33, 45, 60, 78
// Mbit/s, second by second; the sum 72.6 against 90.1). Till the socket's
// own queue first passes alphaLow the cap is now C4-lite's, 1.25 times the
// 2 s delivery maximum, which BBR's start-up fills in about a second; the
// first queue ends the start and α takes over.
var TuneAlphaStart = 1
"""),
    ("""	if arming || c.aRate <= 0 {
		c.aRate = math.Max(c.maxDlv(), s.Delivery)
		c.aDlv = [alphaRecent]float64{}
	}
	c.aQ = 0
	if s.SRTT > 0 && s.MinRTT > 0 && s.SRTT > s.MinRTT {
		c.aQ = s.SRTT - s.MinRTT
	}""", """	if arming || c.aRate <= 0 {
		c.aRate = math.Max(c.maxDlv(), s.Delivery)
		c.aDlv = [alphaRecent]float64{}
		c.aStart = TuneAlphaStart != 0
	}
	owdNoteRate(long)
	c.aQ, c.aOWD = 0, false
	switch {
	case s.OWDok:
		c.aQ, c.aOWD = max(s.OWD, 0), true
	case s.SRTT > 0 && s.MinRTT > 0 && s.SRTT > s.MinRTT:
		c.aQ = s.SRTT - s.MinRTT
	}"""),
    ("""	ceil := wcNetKOf() * c.maxDlv()
	if c.aQ > alphaLow && c.aQ > c.aQ0+alphaRise {""", """	ceil := wcNetKOf() * c.maxDlv()
	if c.aStart && (c.aOWD || s.SRTT > 0 && s.MinRTT > 0) {
		if c.aQ <= alphaLow {
			// fix 1: no queue of its own yet, C4-lite's cap
			c.aRate = math.Max(c.aRate, ceil)
			c.aGrows++
			c.aQ0 = c.aQ
			c.aRate = math.Max(c.aRate, math.Max(alphaKMin*long, alphaFloor))
			return
		}
		c.aStart = false
	}
	if c.aQ > alphaLow && c.aQ > c.aQ0+alphaRise {"""),
    ("""	switch {
	case s.SRTT <= 0 || s.MinRTT <= 0:
		c.aRate = ceil // no queue reading: C4-lite's""", """	// fix 2: a one-way delay is this direction's queue alone, so a growing
	// one is this direction's to answer whatever its share of the bytes
	mine := share >= alphaMajor || c.aOWD
	switch {
	case !c.aOWD && (s.SRTT <= 0 || s.MinRTT <= 0):
		c.aRate = ceil // no queue reading: C4-lite's"""),
    ("""	case c.aUp >= alphaRiseN && share >= alphaMajor && (d < alphaMatch*c.aRate || c.aQ > alphaBig):""",
     """	case c.aUp >= alphaRiseN && mine && (d < alphaMatch*c.aRate || c.aQ > alphaBig):"""),
    ("""		l.apply(l.core.Update(capSample{Now: now, Delivery: float64(ts.deliveryRate), AppLimited: app,
			Acked: ts.bytesAcked, Written: written, Segs: ts.segsOut, SRTT: usDur(uint64(ts.rtt)),
			MinRTT: usDur(uint64(ts.minRTT)), Received: ts.bytesReceived}))""",
     """		var oq time.Duration
		ook := false
		if l.core.alpha {
			oq, ook = owdQueue()
		}
		l.apply(l.core.Update(capSample{Now: now, Delivery: float64(ts.deliveryRate), AppLimited: app,
			Acked: ts.bytesAcked, Written: written, Segs: ts.segsOut, SRTT: usDur(uint64(ts.rtt)),
			MinRTT: usDur(uint64(ts.minRTT)), Received: ts.bytesReceived, OWD: oq, OWDok: ook}))"""),
    ("""		return fmt.Sprintf("sendcap link %d: alpha cap %.1f Mbit/s (queue %.1f ms, target %.0f ms; cuts %d, grows %d), app-limited %.0f%%, idle-lifted %d",
			n, float64(c.set)*8/1e6, msOf(c.aQ), msOf(alphaLow), c.aCuts, c.aGrows, share, c.lifts)""",
     """		src := "srtt"
		if c.aOWD {
			src = "one-way"
		}
		start := ""
		if c.aStart {
			start = ", start"
		}
		return fmt.Sprintf("sendcap link %d: alpha cap %.1f Mbit/s (queue %.1f ms %s, target %.0f ms; cuts %d, grows %d%s), app-limited %.0f%%, idle-lifted %d",
			n, float64(c.set)*8/1e6, msOf(c.aQ), src, msOf(alphaLow), c.aCuts, c.aGrows, start, share, c.lifts)"""),
])

patch("engine/stream_iran.go", [
    ("""		go openControl(ctx, l, logf)
""", """		go openControl(ctx, l, logf)
		go openOWD(ctx, l) // a QUIET link: α's one-way delays (owd.go)
"""),
])

patch("engine/stream_kharej.go", [
    ("""	case kindCtrl:
		serveControl(ctx, st, car)
""", """	case kindCtrl:
		serveControl(ctx, st, car)
	case kindOWD:
		owdLoop(ctx, st) // α's one-way delays on a QUIET link (owd.go)
"""),
])

patch("cmd/hs2/main.go", [
    ("""	num("HS2_TUNE_ALPHA", &engine.TuneAlpha) // α's cap on BULK links (0: C4-lite's there too)
""", """	num("HS2_TUNE_ALPHA", &engine.TuneAlpha) // α's cap on BULK links (0: C4-lite's there too)
	// the judge's fixes to α: C4-lite's cap till the first queue of its
	// own (0: β2's slow start), and the QUIET link's one-way delay as its
	// queue (0: srtt − min_rtt)
	num("HS2_TUNE_ALPHA_START", &engine.TuneAlphaStart)
	num("HS2_TUNE_OWD", &engine.TuneOWD)
"""),
])
print("patched")

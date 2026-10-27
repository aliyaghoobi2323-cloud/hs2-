#!/usr/bin/env python3
"""patch_owd5.py HS2SRC: fix 1 only with fix 2: the start is judged on the
link's one-way delay alone; with HS2_TUNE_OWD=0 (srtt − min_rtt, which
holds the other direction's queue) α is β2's as it was."""
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
    ("""		c.aStart = TuneAlphaStart != 0
""", """		c.aStart = TuneAlphaStart != 0 && !OWDOff()
"""),
    ("""	if c.aStart && (c.aOWD || s.OWDwait || s.SRTT > 0 && s.MinRTT > 0) {""",
     """	if c.aStart && (c.aOWD || s.OWDwait) {"""),
    ("""// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab): α starts from the
// delivery rate it armed at and grows a few % a reading, as β2 had it.
""", """// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab): α starts from the
// delivery rate it armed at and grows a few % a reading, as β2 had it. The
// start is judged on the link's one-way delay (fix 2) alone: with
// HS2_TUNE_OWD=0 α is β2's (srtt − min_rtt holds the other direction's
// queue, and β2's two-way loop test held its queue under 15 ms that way).
"""),
])

patch("engine/quiet_test.go", [
    ("""	s := &sendCap{linked: true, alpha: true}
	feed(s, rate, 2*ms, false)
	feed(s, rate, 2*ms, false) // armed, no queue of its own: C4-lite's cap
	if !s.aStart || !near(s.aRate, wcNetKOf()*rate) {
		t.Fatalf("start: cap %.0f (start %v), want C4-lite's %.0f", s.aRate, s.aStart, wcNetKOf()*rate)
	}
	feed(s, rate, 9*ms, false) // a queue past 5 ms: the start ends, the rules below hold""",
     """	// (the start is judged on the link's one-way delay alone)
	feedOWD := func(c *sendCap, dlv float64, q time.Duration, app bool) {
		at += 100 * ms
		acked += uint64(dlv / 10)
		c.Update(capSample{Now: at, Delivery: dlv, AppLimited: app, Acked: acked, Written: acked, Segs: uint32(acked / 1000),
			SRTT: 72*ms + q, MinRTT: 72 * ms, OWD: q, OWDok: true})
	}
	s := &sendCap{linked: true, alpha: true}
	feedOWD(s, rate, 2*ms, false)
	feedOWD(s, rate, 2*ms, false) // armed, no queue of its own: C4-lite's cap
	if !s.aStart || !near(s.aRate, wcNetKOf()*rate) {
		t.Fatalf("start: cap %.0f (start %v), want C4-lite's %.0f", s.aRate, s.aStart, wcNetKOf()*rate)
	}
	feedOWD(s, rate, 9*ms, false) // a queue past 5 ms: the start ends, the rules below hold"""),
])
print("patched")

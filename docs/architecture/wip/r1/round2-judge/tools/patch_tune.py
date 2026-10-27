#!/usr/bin/env python3
"""patch_tune.py HS2SRC: the judge's α tuning for a shared FIFO (switches
HS2_TUNE_ALPHA_HALF, HS2_TUNE_ALPHA_PLATEAU, default on): with the one-way
delay each direction holds half of CoDel's target, and the cap grows slowly
within 5% of the rate it was cut from last."""
import sys

root = sys.argv[1]
p = root + "/engine/sendcap.go"
s = open(p, encoding="utf-8").read()


def rep(old, new, n=1):
    global s
    if s.count(old) != n:
        sys.exit("sendcap.go: %d matches for %r" % (s.count(old), old[:80]))
    s = s.replace(old, new)


rep("""// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab):""", """// AlphaHalf, AlphaPlateau 0 (HS2_TUNE_ALPHA_HALF=0, HS2_TUNE_ALPHA_PLATEAU=0;
// lab): the judge's FIFO tuning off. On a shared FIFO a light flow's round
// trip crosses both directions' queues, and with the one-way delay each
// direction answered for its own at CoDel's 5 ms: the probe of the deep
// FIFO cell (25 Mbit/s, 600 packets) at +27 ms over its floor against +23
// on srtt, whose sum both ends held near 5 ms together. So with the
// one-way delay each direction holds half the target (alphaLow/2, and
// alphaBig/2 for a big one), and the cap grows by alphaGrowNear only
// within alphaNear of the rate it was cut from last (the path's rate, as
// far as it knows): the few % a reading past it are the queue's peaks.
var AlphaHalf, AlphaPlateau = 1, 1

const (
	alphaGrowNear = 0.005
	alphaNear     = 0.95
)

// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab):""")
rep("""	aStart, aOWD bool
""", """	aStart, aOWD bool
	// aCutFrom: the cap before its latest cut (0: none yet), the plateau's
	aCutFrom float64
""")
# the targets: half with the one-way delay
rep("""	ceil := wcNetKOf() * c.maxDlv()
	// fix 1: till""", """	ceil := wcNetKOf() * c.maxDlv()
	low, big := alphaLow, alphaBig
	if c.aOWD && AlphaHalf != 0 {
		low, big = alphaLow/2, alphaBig/2
	}
	// fix 1: till""")
rep("""		if c.aQ <= alphaLow || s.OWDwait {""", """		if c.aQ <= low || s.OWDwait {""")
rep("""		cut := math.Min(math.Max(float64(c.aQ-alphaLow), 0)/float64(alphaDrain), alphaCutMax)
		c.aRate = math.Min(c.aRate, d) * (1 - cut)
		c.aCuts++""", """		cut := math.Min(math.Max(float64(c.aQ-low), 0)/float64(alphaDrain), alphaCutMax)
		c.aCutFrom = math.Min(c.aRate, d)
		c.aRate = c.aCutFrom * (1 - cut)
		c.aCuts++""")
rep("""	if c.aQ > alphaLow && c.aQ > c.aQ0+alphaRise {""", """	if c.aQ > low && c.aQ > c.aQ0+alphaRise {""")
rep("""	cut := math.Min(math.Max(float64(c.aQ-alphaLow), 0)/float64(alphaDrain), alphaCutMax)
	// fix 2""", """	cut := math.Min(math.Max(float64(c.aQ-low), 0)/float64(alphaDrain), alphaCutMax)
	// fix 2""")
rep("""	case d < alphaKeep*c.aRate:
		c.aRate = d * (1 - cut)
		if cut > 0 {
			c.aCuts++
		}""", """	case d < alphaKeep*c.aRate:
		c.aRate = d * (1 - cut)
		if cut > 0 {
			c.aCutFrom = d
			c.aCuts++
		}""")
rep("""	case c.aUp >= alphaRiseN && mine && (d < alphaMatch*c.aRate || c.aQ > alphaBig):
		c.aRate = math.Min(c.aRate, d) * (1 - cut)
		c.aCuts++""", """	case c.aUp >= alphaRiseN && mine && (d < alphaMatch*c.aRate || c.aQ > big):
		c.aCutFrom = math.Min(c.aRate, d)
		c.aRate = c.aCutFrom * (1 - cut)
		c.aCuts++""")
rep("""		g := alphaGrow
		if c.aQ < alphaLow {
			g = alphaGrowFast
		}""", """		g := alphaGrow
		if c.aQ < low {
			g = alphaGrowFast
		}
		if c.aOWD && AlphaPlateau != 0 && c.aCutFrom > 0 && c.aRate >= alphaNear*c.aCutFrom && c.aRate <= c.aCutFrom/alphaNear {
			g = alphaGrowNear // near the rate it was cut from: the path's
		}""")
open(p, "w", encoding="utf-8", newline="\n").write(s)

m = root + "/cmd/hs2/main.go"
t = open(m, encoding="utf-8").read()
old = """	num("HS2_TUNE_OWD", &engine.TuneOWD)
"""
if t.count(old) != 1:
    sys.exit("main.go")
t = t.replace(old, old + """	num("HS2_TUNE_ALPHA_HALF", &engine.AlphaHalf)
	num("HS2_TUNE_ALPHA_PLATEAU", &engine.AlphaPlateau)
""")
open(m, "w", encoding="utf-8", newline="\n").write(t)
print("patched")

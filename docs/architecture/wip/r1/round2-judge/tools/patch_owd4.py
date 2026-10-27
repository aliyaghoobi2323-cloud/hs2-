#!/usr/bin/env python3
"""patch_owd4.py HS2SRC: fix 1, second version: the start is judged before
the app-limited hold, and the first queue of its own ends it with a cut to
what was delivered (the start had held for good on app-limited readings)."""
import sys

root = sys.argv[1]
p = root + "/engine/sendcap.go"
s = open(p, encoding="utf-8").read()
old = """	if s.AppLimited && d < alphaKeep*c.aRate {
		return // the sender had less to send than the cap let out: it holds
	}
	ceil := wcNetKOf() * c.maxDlv()
	if c.aStart && (c.aOWD || s.OWDwait || s.SRTT > 0 && s.MinRTT > 0) {
		if c.aQ <= alphaLow || s.OWDwait {
			// fix 1: no queue of its own yet, C4-lite's cap
			c.aRate = math.Max(c.aRate, ceil)
			c.aGrows++
			c.aQ0 = c.aQ
			c.aRate = math.Max(c.aRate, math.Max(alphaKMin*long, alphaFloor))
			return
		}
		c.aStart = false
	}
"""
new = """	ceil := wcNetKOf() * c.maxDlv()
	// fix 1: till its own queue first passes alphaLow the cap is C4-lite's,
	// whatever Linux's app-limited bit (under C4-lite's 1.25 x a sender
	// fed by a 20 KiB lowat finds its queue empty on most samples: the
	// judge's first version held the start, and a download's standing
	// queue past 300 ms, for good behind the app-limited hold below); the
	// first queue of its own ends it, the cap at what was delivered less
	// the share that drains the queue past alphaLow
	if c.aStart && (c.aOWD || s.OWDwait || s.SRTT > 0 && s.MinRTT > 0) {
		if c.aQ <= alphaLow || s.OWDwait {
			c.aRate = math.Max(ceil, alphaFloor)
			c.aGrows++
			c.aQ0 = c.aQ
			return
		}
		c.aStart = false
		cut := math.Min(math.Max(float64(c.aQ-alphaLow), 0)/float64(alphaDrain), alphaCutMax)
		c.aRate = math.Min(c.aRate, d) * (1 - cut)
		c.aCuts++
		c.aQ0 = c.aQ
		c.aRate = math.Max(c.aRate, math.Max(alphaKMin*long, alphaFloor))
		return
	}
	if s.AppLimited && d < alphaKeep*c.aRate {
		return // the sender had less to send than the cap let out: it holds
	}
"""
if s.count(old) != 1:
    sys.exit("sendcap.go: %d matches" % s.count(old))
s = s.replace(old, new)
open(p, "w", encoding="utf-8", newline="\n").write(s)
print("patched")

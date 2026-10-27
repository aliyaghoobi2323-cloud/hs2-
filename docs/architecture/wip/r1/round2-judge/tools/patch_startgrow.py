#!/usr/bin/env python3
"""patch_startgrow.py HS2SRC: the start ends at the first queue of its own
only once the delivery rate stopped growing (BBR's start-up queue while it
still doubles is no path rate); free growth off by default (the two-way
loop model oscillated with it: queue 9.8 ms, 0.97 of the paths)."""
import sys

root = sys.argv[1]
p = root + "/engine/sendcap.go"
s = open(p, encoding="utf-8").read()


def rep(old, new):
    global s
    if s.count(old) != 1:
        sys.exit("sendcap.go: %d matches for %r" % (s.count(old), old[:80]))
    s = s.replace(old, new)


rep("""var AlphaFree = 1
""", """//
// Off by default: in the two-way loop model it made the cap overshoot after
// every drained cut (a queue to 9.8 ms and 0.97 of the paths, against 5.9
// ms and 0.99 without); the start's own rule (alphaStartGrow) took its
// place.
var AlphaFree = 0
""")
rep("""const (
	alphaFreeN    = 5
	alphaGrowFree = 0.1
)
""", """const (
	alphaFreeN    = 5
	alphaGrowFree = 0.1
	// alphaStartGrow: the start does not end while the delivery rate grew
	// by this over the last alphaRecent−1 readings (BBR's start-up: it
	// doubles each round trip, and its queue then is no sign of the path's
	// rate; in F2 a start ended at 12 Mbit/s of 100 and the cap stayed
	// there 10 s)
	alphaStartGrow = 1.25
)
""")
rep("""		if c.aQ <= low || s.OWDwait {
			c.aRate = math.Max(ceil, alphaFloor)""", """		// growing: the delivery rose by alphaStartGrow since the oldest
		// reading of the last alphaRecent (fewer than two since it armed:
		// it has just started, as BBR's start-up)
		growing, n, old := true, 0, 0.0
		for k := 1; k <= alphaRecent; k++ {
			if v := c.aDlv[(c.aDi+k)%alphaRecent]; v > 0 {
				if n++; old == 0 {
					old = v
				}
			}
		}
		if n >= 2 {
			growing = s.Delivery >= alphaStartGrow*old
		}
		if c.aQ <= low || s.OWDwait || growing {
			c.aRate = math.Max(ceil, alphaFloor)""")
open(p, "w", encoding="utf-8", newline="\n").write(s)
print("patched")

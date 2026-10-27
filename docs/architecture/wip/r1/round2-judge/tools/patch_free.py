#!/usr/bin/env python3
"""patch_free.py HS2SRC: the judge's free growth (HS2_TUNE_ALPHA_FREE,
default on): with the one-way delay, a cap whose own queue stayed under a
quarter of the target for alphaFreeN readings grows by alphaGrowFree a
reading (to C4-lite's ceiling at most) and forgets the rate it was cut from."""
import sys

root = sys.argv[1]
p = root + "/engine/sendcap.go"
s = open(p, encoding="utf-8").read()


def rep(old, new):
    global s
    if s.count(old) != 1:
        sys.exit("sendcap.go: %d matches for %r" % (s.count(old), old[:80]))
    s = s.replace(old, new)


rep("""var AlphaHalf, AlphaPlateau = 1, 1
""", """var AlphaHalf, AlphaPlateau = 1, 1

// AlphaFree 0 (HS2_TUNE_ALPHA_FREE=0, lab): the judge's free growth off.
// The start ends at the first queue of its own, and BBR's start-up can make
// one while it still delivers a tenth of the path: then the cap climbed by
// 3% a reading from there (a download beside an upload on a deep FIFO at
// 6, 8, 9, 11, 12, 14, 16, 18, 20 Mbit/s second by second, 77 at second
// 19). With the one-way delay, a queue under a quarter of the target for
// alphaFreeN readings in a row means the path has room: the cap grows by
// alphaGrowFree a reading (C4-lite's ceiling at most) and the rate it was
// cut from no longer holds it back.
var AlphaFree = 1

const (
	alphaFreeN    = 5
	alphaGrowFree = 0.1
)
""")
rep("""	// aCutFrom: the cap before its latest cut (0: none yet), the plateau's
	aCutFrom float64
""", """	// aCutFrom: the cap before its latest cut (0: none yet), the plateau's;
	// aFree: readings in a row its own queue stayed under a quarter of the
	// target (free growth)
	aCutFrom float64
	aFree    int
""")
rep("""	if c.aQ > low && c.aQ > c.aQ0+alphaRise {""", """	if c.aOWD && c.aQ < low/4 {
		c.aFree++
	} else {
		c.aFree = 0
	}
	if c.aQ > low && c.aQ > c.aQ0+alphaRise {""")
rep("""		if c.aOWD && AlphaPlateau != 0 && c.aCutFrom > 0 && c.aRate >= alphaNear*c.aCutFrom && c.aRate <= c.aCutFrom/alphaNear {
			g = alphaGrowNear // near the rate it was cut from: the path's
		}""", """		if c.aOWD && AlphaPlateau != 0 && c.aCutFrom > 0 && c.aRate >= alphaNear*c.aCutFrom && c.aRate <= c.aCutFrom/alphaNear {
			g = alphaGrowNear // near the rate it was cut from: the path's
		}
		if c.aOWD && AlphaFree != 0 && c.aFree >= alphaFreeN {
			g = math.Max(g, alphaGrowFree) // no queue for a while: room on the path
			c.aCutFrom = 0
		}""")
open(p, "w", encoding="utf-8", newline="\n").write(s)

m = root + "/cmd/hs2/main.go"
t = open(m, encoding="utf-8").read()
old = """	num("HS2_TUNE_ALPHA_PLATEAU", &engine.AlphaPlateau)
"""
if t.count(old) != 1:
    sys.exit("main.go")
t = t.replace(old, old + """	num("HS2_TUNE_ALPHA_FREE", &engine.AlphaFree)
""")
open(m, "w", encoding="utf-8", newline="\n").write(t)
print("patched")

#!/usr/bin/env python3
"""patch_extra.py HS2SRC: the judge's switch HS2_TUNE_QUIET_EXTRA=1, QUIET
links next to max_links BULK ones instead of within max_links."""
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


patch("engine/quiet.go", [
    ("""// QuietTotal is the most links a pool of maxLinks holds with its QUIET ones
// (they count toward max_links, but a pool of one link still has a BULK
// one: one more then).
func QuietTotal(maxLinks int) int { return max(maxLinks, 1+QuietCount(maxLinks)) }
""", """// QuietExtra 1 (HS2_TUNE_QUIET_EXTRA=1; the R1 round-2 judge): the QUIET
// links come next to max_links BULK ones, not within max_links. Within
// them, a pool of N links under a policer on each connection (a per-flow
// limit on the path) carries bulk on N − 2 (N ≥ 4; N − 1 below): on the
// rig, N=8 under a 10 Mbit/s policer each, a lone user's 16 connections got
// 55 Mbit/s against 72 for the base binary and 74 for β1 (whose QUIET
// connection is a link's second). The cost is one or two connections more
// than max_links. Both ends set it alike (the exit dials the reverse pool's
// links: QuietTotal).
var QuietExtra = 0

// QuietTotal is the most links a pool of maxLinks holds with its QUIET ones
// (they count toward max_links, but a pool of one link still has a BULK
// one: one more then; QuietExtra: maxLinks BULK ones and the QUIET ones).
func QuietTotal(maxLinks int) int {
	if QuietExtra != 0 {
		return maxLinks + QuietCount(maxLinks)
	}
	return max(maxLinks, 1+QuietCount(maxLinks))
}
"""),
    ("""func (m *LinkManager) bulkMax() int {
	if m.quiet.active() {
		return max(1, m.max-m.quiet.want)
	}""", """func (m *LinkManager) bulkMax() int {
	if m.quiet.active() && QuietExtra == 0 {
		return max(1, m.max-m.quiet.want)
	}"""),
])

patch("engine/quiet_test.go", [
    ("""		if _, _, ok := q.pick(nil); ok {
			t.Fatal("a QUIET pick with a level 1 peer")""", """		if _, _, ok := q.pick(nil); ok {
			t.Fatal("a QUIET pick with a level 1 peer")"""),
])

patch("cmd/hs2/main.go", [
    ("""	num("HS2_TUNE_QUIET_REC", &engine.QuietRec) // lab: QUIET's interactive record sizes
""", """	num("HS2_TUNE_QUIET_REC", &engine.QuietRec) // lab: QUIET's interactive record sizes
	num("HS2_TUNE_QUIET_EXTRA", &engine.QuietExtra) // QUIET links next to max_links BULK ones
"""),
])
print("patched")

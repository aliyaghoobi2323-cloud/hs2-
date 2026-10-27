#!/usr/bin/env python3
"""patch_rec.py HS2SRC: the judge's lab switch HS2_TUNE_QUIET_REC=1, an
interactive-HTTPS record-size sampler on QUIET links (obfs, engine, cmd)."""
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


patch("obfs/shaper.go", [
    ("""// Sample returns a target on-wire size.""", """// NewInteractiveLengthSampler returns a sampler of the record sizes of an
// interactive HTTPS connection (requests, small replies, HTTP/2 control):
// mostly small records with a tail of full ones, the mean ~390 B against the
// bulk sampler's ~990 B. The R1 round-2 judge's lab choice for QUIET links
// (HS2_TUNE_QUIET_REC=1): a QUIET link carries small messages, each padded
// up to a sampled size, and with the bulk sampler 198 users' probes (5 a
// second each, 32 B) cost 7.5 Mbit/s of padding each way on the rig. Not a
// default: its DPI footprint is the DPI review's to judge.
func NewInteractiveLengthSampler() *LengthSampler {
	pairs := []struct {
		size   int
		weight float64
	}{
		{1400, 0.10}, {1200, 0.04}, {900, 0.04}, {600, 0.06},
		{400, 0.10}, {250, 0.16}, {150, 0.18}, {80, 0.18}, {40, 0.14},
	}
	s := &LengthSampler{}
	var total float64
	for _, p := range pairs {
		total += p.weight
	}
	var acc float64
	for _, p := range pairs {
		acc += p.weight / total
		s.sizes = append(s.sizes, p.size)
		s.cum = append(s.cum, acc)
	}
	return s
}

// Sample returns a target on-wire size."""),
])

patch("engine/stream.go", [
    ("""	shaped := newShapedConn(bottom, sampler)
	shaped.carry = sched""", """	if q, _ := classOf(caps); q && QuietRec != 0 {
		sampler = obfs.NewInteractiveLengthSampler() // (lab: QUIET's records, quiet.go)
	}
	shaped := newShapedConn(bottom, sampler)
	shaped.carry = sched"""),
])

patch("engine/quiet.go", [
    ("""var QuietCC = "cubic"
""", """var QuietCC = "cubic"

// QuietRec 1 (HS2_TUNE_QUIET_REC=1, lab; the R1 round-2 judge): a QUIET
// link's records follow an interactive HTTPS connection's sizes
// (obfs.NewInteractiveLengthSampler), not bulk HTTPS's: each small message
// is padded to ~390 B on average instead of ~990 B. The other end decodes
// any size, so each end's switch is its own sending's.
var QuietRec = 0
"""),
])

patch("cmd/hs2/main.go", [
    ("""	num("HS2_TUNE_OWD", &engine.TuneOWD)
""", """	num("HS2_TUNE_OWD", &engine.TuneOWD)
	num("HS2_TUNE_QUIET_REC", &engine.QuietRec) // lab: QUIET's interactive record sizes
"""),
])
print("patched")

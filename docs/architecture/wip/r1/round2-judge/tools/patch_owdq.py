#!/usr/bin/env python3
"""patch_owdq.py HS2SRC: fix 2, fifth version (HS2_TUNE_OWD_QUIET, default
on): the one-way queue α reads is the QUIET link's in the same direction
(what the light flows wait in), shared by every BULK link of the process;
the beat follows the BULK links' rate."""
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
    ("""// TuneOWD 0 (HS2_TUNE_OWD=0): α reads srtt − min_rtt (β2's).
var TuneOWD = 1
""", """// TuneOWD 0 (HS2_TUNE_OWD=0): α reads srtt − min_rtt (β2's).
var TuneOWD = 1

// TuneOWDQuiet 0 (HS2_TUNE_OWD_QUIET=0): each BULK link trades its own
// messages and α reads its own link's queue (version 4). On (version 5,
// the default): the edge opens kindOWD on the QUIET links and every BULK
// link's α reads the QUIET link's one-way queue of its own direction —
// the queue the light flows wait in. Version 4 on fq_codel, where each
// BULK link has a flow queue of its own the light flows never see, cut
// each link for its own queue: with 17 flows on 64 links the sum fell
// from 89 to 50 Mbit/s over 30 s, and of two uploads on two links one
// held 65 Mbit/s, the other 9 (F3). There a QUIET link sees no queue and
// α is C4-lite; on a shared FIFO it sees what each direction's bulk makes.
var TuneOWDQuiet = 1

// quietOWD is the process's QUIET link one-way delays (version 5; nil:
// none up yet), the latest QUIET link's.
var quietOWD atomic.Pointer[owdLink]

// owdShared is what a BULK link's α reads: the QUIET link's (version 5) or
// its own (version 4, own).
func owdShared(own *owdLink) *owdLink {
	if TuneOWDQuiet == 0 {
		return own
	}
	return quietOWD.Load()
}
"""),
    ("""// noteRate is the link's delivery rate (bytes/s) for the beat.
func (o *owdLink) noteRate(r float64) {
	if o == nil {
		return
	}
	o.rate.Store(int64(math.Float64bits(r)))
	o.rateAt.Store(ctrlNow())
}""", """// noteRate is the link's delivery rate (bytes/s) for the beat; a QUIET
// link's (version 5) takes the largest of its BULK links' within a second.
func (o *owdLink) noteRate(r float64) {
	if o == nil {
		return
	}
	now := ctrlNow()
	if at := o.rateAt.Load(); at != 0 && time.Duration(now-at) < time.Second && math.Float64frombits(uint64(o.rate.Load())) > r {
		return
	}
	o.rate.Store(int64(math.Float64bits(r)))
	o.rateAt.Store(now)
}"""),
    ("""// openOWD runs the edge's end of kindOWD on a BULK link (others: none).
func openOWD(ctx context.Context, l Link) {
	ml, ok := l.(*mtcpLink)
	if !ok || !ml.bulk || OWDOff() || ml.mtr == nil {
		return
	}""", """// openOWD runs the edge's end of kindOWD on a QUIET link (version 5) or
// a BULK one (version 4); others: none.
func openOWD(ctx context.Context, l Link) {
	ml, ok := l.(*mtcpLink)
	if !ok || OWDOff() || ml.mtr == nil {
		return
	}
	if TuneOWDQuiet != 0 {
		if !ml.quiet {
			return
		}
		quietOWD.Store(&ml.mtr.owd)
	} else if !ml.bulk {
		return
	}"""),
])

patch("engine/stream_kharej.go", [
    ("""		owdLoop(ctx, st, &mtr.owd) // α's one-way delays on a BULK link (owd.go)""",
     """		if TuneOWDQuiet != 0 && car != nil {
			if q, _ := classOf(car.Caps()); q {
				quietOWD.Store(&mtr.owd) // version 5: the BULK links read it
			}
		}
		owdLoop(ctx, st, &mtr.owd) // α's one-way delays (owd.go)"""),
])

patch("engine/stream.go", [
    ("""			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
				meter.owd.sys = sys
			}""", """			if meter != nil {
				meter.owd.sys = sys // a message's stamp: when it leaves the host
			}
			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
			}"""),
])

patch("engine/sendcap.go", [
    ("""			l.rx0, l.rxAt = ts.bytesReceived, now
			l.owd.noteRate(max(l.core.maxDlv(), rx))
			if l.core.alpha {
				oq, ook = l.owd.queue()
			}""", """			l.rx0, l.rxAt = ts.bytesReceived, now
			src := owdShared(l.owd)
			src.noteRate(max(l.core.maxDlv(), rx))
			if l.core.alpha {
				oq, ook = src.queue()
			}"""),
])

patch("cmd/hs2/main.go", [
    ("""	num("HS2_TUNE_OWD", &engine.TuneOWD)
""", """	num("HS2_TUNE_OWD", &engine.TuneOWD)
	num("HS2_TUNE_OWD_QUIET", &engine.TuneOWDQuiet)
"""),
])
print("patched")

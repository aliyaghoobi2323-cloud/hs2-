package engine

import "testing"

// markSys is a hostSys that keeps the marks set on it.
type markSys struct{ marks []int }

func (m *markSys) setLowat(n int) error  { m.marks = append(m.marks, n); return nil }
func (m *markSys) notsent() (int, error) { return 0, nil }
func (m *markSys) waitBelow(int) error   { return nil }
func (m *markSys) last() int             { return m.marks[len(m.marks)-1] }
func (m *markSys) all() []int            { return m.marks }

// C3's gate variants for the judge (decision T12), read as a guard is made:
// the document's by default (cap max(2 ms, 2 segments), mark max(cap/2,
// MSS), on below 20 Mbit/s); HS2_TUNE_HQG_SEGS=N, the cap at least N
// segments and the mark the cap; HS2_TUNE_HQG_GATE=0, no gate at any rate,
// the dynamic mark alone.
func TestAngleHQGVariants(t *testing.T) {
	const mss, r10 = 1448, 10e6 / 8
	for _, c := range []struct {
		name         string
		segs, gate   int
		on           bool
		cap, lowat   int
	}{
		{"document", 0, 1, true, 2 * mss, mss},
		{"segs4", 4, 1, true, 4 * mss, 4 * mss},
		{"segs8", 8, 1, true, 8 * mss, 8 * mss},
		{"nogate", 0, 0, false, 2 * mss, Lowat(r10, mss)},
	} {
		func() {
			defer func(s, g int) { HQGSegs, HQGGate = s, g }(HQGSegs, HQGGate)
			HQGSegs, HQGGate = c.segs, c.gate
			sys := &markSys{}
			h := newHostQ(nil, sys)
			HQGSegs, HQGGate = 0, 1 // read as the guard was made, not after
			h.Update(r10, mss)
			if h.on.Load() != c.on || int(h.cap.Load()) != c.cap || int(h.lowat.Load()) != c.lowat || sys.last() != c.lowat {
				t.Fatalf("%s: gate %v cap %d mark %d (set %v), want %v %d %d", c.name, h.on.Load(), h.cap.Load(), h.lowat.Load(), sys.all(), c.on, c.cap, c.lowat)
			}
		}()
	}
	// past the gate's rate the variants keep the dynamic mark
	g := hostGate{segs: 8}
	if on, _, lowat := g.Update(40e6/8, mss); on || lowat != Lowat(40e6/8, mss) {
		t.Fatalf("segs8 at 40 Mbit/s: gate %v mark %d", on, lowat)
	}
}

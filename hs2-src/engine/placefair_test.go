package engine

import "testing"

// TestPlaceFair: eight heavy users on a pool of 64 links spread over one link
// each with PlaceFair 8, not eight; a lone heavy user over eight, not 64.
func TestPlaceFair(t *testing.T) {
	spread := func(fair, users int) int {
		withTune(t, PlaceFair, fair)
		withTune(t, PlaceUsers, 2)
		var b placeBook
		cands := make([]*placeCand, 64)
		for i := range cands {
			cands[i] = &placeCand{link: &mtcpLink{}}
		}
		for u := 1; u <= users; u++ {
			due := map[uint32]int{}
			for v := 1; v <= users; v++ {
				due[uint32(v)] = 16
			}
			b.setDue(due)
			for range 16 {
				c := b.choose(cands, uint32(u))
				b.add(c.link, uint32(u))
			}
		}
		b.setDue(nil)
		links := 0
		for _, c := range cands {
			if b.by[c.link][1] > 0 {
				links++
			}
		}
		return links
	}
	for _, c := range []struct{ fair, users, want int }{{0, 8, 8}, {8, 8, 1}, {0, 1, 16}, {8, 1, 8}, {16, 1, 16}, {16, 8, 2}} {
		if got := spread(c.fair, c.users); got != c.want {
			t.Errorf("PlaceFair %d, %d heavy users: the first on %d links, want %d", c.fair, c.users, got, c.want)
		}
	}
}

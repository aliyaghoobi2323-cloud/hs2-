package engine

import "testing"

// HS2_TUNE_QUIET_EXTRA=1 (the R1 round-2 judge; the default from round 3,
// decision T19): max_links BULK links and the QUIET ones next to them, so a
// policer on each connection leaves the bulk all N links; one QUIET link
// whatever N (HS2_TUNE_QUIET_TWO=1: two from N=4 on, as round 2 had it).
func TestQuietCountsExtra(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	if QuietExtra != 1 || QuietTwo != 0 {
		t.Fatalf("defaults: QuietExtra %d, QuietTwo %d; want 1 and 0 (T19)", QuietExtra, QuietTwo)
	}
	check := func(two int, rows []struct{ max, q, total int }) {
		t.Helper()
		QuietTwo = two
		defer func() { QuietTwo = 0 }()
		for _, c := range rows {
			lm := NewLinkManager(nil, 1, c.max, 8, nil)
			q := lm.enableQuiet()
			if q.want != c.q || lm.bulkMax() != c.max || QuietTotal(c.max) != c.total || lm.ctlTarget()-lm.Target() != c.q {
				t.Fatalf("two %d, max_links %d: QUIET %d, BULK max %d, total %d, asked of the exit past the target %d; want %d %d %d",
					two, c.max, q.want, lm.bulkMax(), QuietTotal(c.max), lm.ctlTarget()-lm.Target(), c.q, c.max, c.total)
			}
		}
	}
	check(0, []struct{ max, q, total int }{{1, 1, 2}, {2, 1, 3}, {3, 1, 4}, {4, 1, 5}, {8, 1, 9}, {300, 1, 301}})
	check(1, []struct{ max, q, total int }{{1, 1, 2}, {2, 1, 3}, {3, 1, 4}, {4, 2, 6}, {8, 2, 10}, {300, 2, 302}})
}

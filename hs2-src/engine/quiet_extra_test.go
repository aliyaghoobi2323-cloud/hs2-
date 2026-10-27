package engine

import "testing"

// HS2_TUNE_QUIET_EXTRA=1 (the R1 round-2 judge): max_links BULK links and
// the QUIET ones next to them, so a policer on each connection leaves the
// bulk all N links.
func TestQuietCountsExtra(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	QuietExtra = 1
	defer func() { QuietExtra = 0 }()
	for _, c := range []struct{ max, q, total int }{{1, 1, 2}, {2, 1, 3}, {3, 1, 4}, {4, 2, 6}, {8, 2, 10}, {300, 2, 302}} {
		lm := NewLinkManager(nil, 1, c.max, 8, nil)
		q := lm.enableQuiet()
		if q.want != c.q || lm.bulkMax() != c.max || QuietTotal(c.max) != c.total || lm.ctlTarget()-lm.Target() != c.q {
			t.Fatalf("max_links %d: QUIET %d, BULK max %d, total %d, asked of the exit past the target %d; want %d %d %d",
				c.max, q.want, lm.bulkMax(), QuietTotal(c.max), lm.ctlTarget()-lm.Target(), c.q, c.max, c.total)
		}
	}
}

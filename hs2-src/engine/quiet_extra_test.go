package engine

import "testing"

// HS2_TUNE_QUIET_EXTRA=1 (the R1 round-2 judge; the default since round 3,
// decision T19): max_links BULK links and one QUIET link next to them (two
// past five links), so a policer on each connection leaves the bulk all N
// links; HS2_TUNE_QUIET_N=2 (lab) keeps two QUIET links whatever N.
func TestQuietCountsExtra(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	defer func(x, n int) { QuietExtra, QuietN = x, n }(QuietExtra, QuietN)
	QuietExtra, QuietN = 1, 0
	check := func(cases []struct{ max, q, total int }) {
		t.Helper()
		for _, c := range cases {
			lm := NewLinkManager(nil, 1, c.max, 8, nil)
			q := lm.enableQuiet()
			if q.want != c.q || lm.bulkMax() != c.max || QuietTotal(c.max) != c.total || lm.ctlTarget()-lm.Target() != c.q {
				t.Fatalf("QuietN %d, max_links %d: QUIET %d, BULK max %d, total %d, asked of the exit past the target %d; want %d %d %d",
					QuietN, c.max, q.want, lm.bulkMax(), QuietTotal(c.max), lm.ctlTarget()-lm.Target(), c.q, c.max, c.total)
			}
		}
	}
	check([]struct{ max, q, total int }{{1, 1, 2}, {2, 1, 3}, {3, 1, 4}, {4, 1, 5}, {5, 1, 6}, {6, 2, 8}, {8, 2, 10}, {64, 2, 66}, {300, 2, 302}})
	QuietN = 2
	check([]struct{ max, q, total int }{{1, 2, 3}, {4, 2, 6}, {300, 2, 302}})
}

// The defaults of round 3: QUIET outside max_links, one QUIET link up to
// five links, two past.
func TestQuietDefaultsRound3(t *testing.T) {
	if QuietExtra != 1 || QuietN != 0 || QuietBatch != 1 || QuietCount(5) != 1 || QuietCount(6) != 2 {
		t.Fatalf("QuietExtra %d, QuietN %d, QuietBatch %d, QuietCount(5) %d, (6) %d; want 1 0 1 1 2", QuietExtra, QuietN, QuietBatch, QuietCount(5), QuietCount(6))
	}
}

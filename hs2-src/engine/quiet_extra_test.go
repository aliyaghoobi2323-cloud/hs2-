package engine

import "testing"

// HS2_TUNE_QUIET_EXTRA=1 (the R1 round-2 judge; the default since round 3,
// decision T19): max_links BULK links and QUIET links next to them (two up
// to five links since the round-4 judge, QuietFew; four past five, round
// 4), so a policer on each connection leaves the bulk all N links;
// HS2_TUNE_QUIET_N=2 (lab) keeps two QUIET links whatever N.
func TestQuietCountsExtra(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	defer func(x, n, f int) { QuietExtra, QuietN, QuietFew = x, n, f }(QuietExtra, QuietN, QuietFew)
	QuietExtra, QuietN, QuietFew = 1, 0, 1
	withTune(t, QuietLight, 0) // the light link's count: TestQuietCountLight
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
	check([]struct{ max, q, total int }{{1, 1, 2}, {2, 1, 3}, {3, 1, 4}, {4, 1, 5}, {5, 1, 6}, {6, 4, 10}, {8, 4, 12}, {64, 4, 68}, {300, 4, 304}})
	// the round-4 judge's two up to five links (HS2_TUNE_QUIET_FEW=2)
	QuietFew = 2
	check([]struct{ max, q, total int }{{1, 2, 3}, {5, 2, 7}, {6, 4, 10}})
	QuietFew = 1
	// round 3's two past five links (HS2_TUNE_QUIET_MANY=2)
	defer func(m int) { QuietMany = m }(QuietMany)
	QuietMany = 2
	check([]struct{ max, q, total int }{{5, 1, 6}, {6, 2, 8}, {8, 2, 10}, {300, 2, 302}})
	QuietMany = 4
	QuietN = 2
	check([]struct{ max, q, total int }{{1, 2, 3}, {4, 2, 6}, {300, 2, 302}})
}

// The defaults (round 3; round 4 four QUIET links past five; the round-4
// judge kept one up to five; round 5 one light link more, but at five):
// QUIET outside max_links, two QUIET links up to five links, five past.
func TestQuietDefaultsRound3(t *testing.T) {
	if QuietExtra != 1 || QuietN != 0 || QuietBatch != 1 || QuietCount(4) != 2 || QuietCount(5) != 2 || QuietCount(6) != 5 {
		t.Fatalf("QuietExtra %d, QuietN %d, QuietBatch %d, QuietCount(4) %d, (5) %d, (6) %d; want 1 0 1 2 2 5", QuietExtra, QuietN, QuietBatch, QuietCount(4), QuietCount(5), QuietCount(6))
	}
}

// The rest of round 5's defaults (round5/REPORT.fa.md): what was measured.
func TestDefaultsRound5(t *testing.T) {
	if QuietCC != "illinois,cubic" || QuietCCLoss != "" {
		t.Errorf("QuietCC %q, QuietCCLoss %q; want illinois,cubic and none", QuietCC, QuietCCLoss)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"HS2_TUNE_QUIET_LIGHT", QuietLight.Get(), 3},
		{"HS2_TUNE_BUDGET_DFLOOR", BudgetDFloor.Get(), 100},
		{"HS2_TUNE_PLACE_FAIR", PlaceFair.Get(), 8},
		{"HS2_TUNE_QUIET_WAIT", QuietWaitMs.Get(), 1500},
		{"HS2_TUNE_QUIET_REPLACE", QuietReplace.Get(), 1},
		{"HS2_TUNE_QUIET_BUDGET", QuietBudget.Get(), 1},
		{"HS2_TUNE_OWD_GUARD_BULK", OWDGuardBulk, 0},
		{"HS2_TUNE_RT_LOOSE", RTLoose.Get(), 0},
		{"HS2_TUNE_OWD_GUARD_IN", OWDGuardIn.Get(), 0},
		{"HS2_TUNE_OWD_NOT_LIGHT", OWDNotLight.Get(), 0},
		{"HS2_TUNE_BUDGET_SPLIT", BudgetSplitRate, 0},
	} {
		if c.got != c.want {
			t.Errorf("%s %d, want %d", c.name, c.got, c.want)
		}
	}
}

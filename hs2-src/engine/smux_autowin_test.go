package engine

import "testing"

func TestAutoWinFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if aw := autoWinFromEnv(env(nil)); aw != nil {
		t.Fatal("off by default")
	}
	if aw := autoWinFromEnv(env(map[string]string{"HS2_SMUX_AUTOWIN": "0"})); aw != nil {
		t.Fatal("0 is off")
	}
	aw := autoWinFromEnv(env(map[string]string{"HS2_SMUX_AUTOWIN": "1"}))
	if aw == nil || aw.Max != 32<<20 || aw.UpdateDiv != 8 || aw.Budget == nil {
		t.Fatalf("defaults: %+v", aw)
	}
	if l := aw.Budget.Stats().Limit; l > 512<<20 || l < 64<<20 {
		t.Fatalf("default budget %d", l)
	}
	aw = autoWinFromEnv(env(map[string]string{"HS2_SMUX_AUTOWIN": "1", "HS2_SMUX_AUTOWIN_MAX_MB": "16",
		"HS2_SMUX_AUTOWIN_DIV": "8", "HS2_SMUX_AUTOWIN_BUDGET_MB": "100"}))
	if aw.Max != 16<<20 || aw.UpdateDiv != 8 || aw.Budget.Stats().Limit != 100<<20 {
		t.Fatalf("overrides: %+v", aw)
	}
	if aw.QueueScale != 0.3 {
		t.Fatalf("queue scale default %v", aw.QueueScale)
	}
	if aw = autoWinFromEnv(env(map[string]string{"HS2_SMUX_AUTOWIN": "1", "HS2_SMUX_AUTOWIN_QUEUE_PCT": "50"})); aw.QueueScale != 0.5 {
		t.Fatalf("queue pct: %v", aw.QueueScale)
	}
	aw = autoWinFromEnv(env(map[string]string{"HS2_SMUX_AUTOWIN": "1", "HS2_SMUX_AUTOWIN_MAX_MB": "x", "HS2_SMUX_AUTOWIN_DIV": "1"}))
	if aw.Max != 32<<20 || aw.UpdateDiv != 8 {
		t.Fatalf("bad values fall back: %+v", aw)
	}
}

// With the switch off the sessions get no AutoWindow: upstream smux.
func TestSmuxConfigOffHasNoAutoWindow(t *testing.T) {
	if smuxAutoWin != nil {
		t.Skip("HS2_SMUX_AUTOWIN set in the test environment")
	}
	if newSmuxConfig().AutoWindow != nil {
		t.Fatal("AutoWindow set with the switch off")
	}
}

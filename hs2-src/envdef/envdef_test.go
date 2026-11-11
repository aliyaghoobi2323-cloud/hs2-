package envdef

import (
	"os"
	"testing"
)

func unset(t *testing.T, k string) {
	t.Helper()
	old, had := os.LookupEnv(k)
	os.Unsetenv(k)
	t.Cleanup(func() {
		if had {
			os.Setenv(k, old)
		} else {
			os.Unsetenv(k)
		}
	})
}

func TestDormantUntilActivate(t *testing.T) {
	Deactivate()
	t.Cleanup(Deactivate)
	unset(t, "HS2_MCONN")
	unset(t, "HS2_DEFAULTS")
	if v := Get("HS2_MCONN"); v != "" {
		t.Fatalf("dormant: HS2_MCONN=%q, want empty", v)
	}
	if !Activate(true) {
		t.Fatal("Activate returned false")
	}
	if v := Get("HS2_MCONN"); v != "lite" {
		t.Fatalf("active: HS2_MCONN=%q, want lite", v)
	}
}

func TestSides(t *testing.T) {
	t.Cleanup(Deactivate)
	unset(t, "HS2_DEFAULTS")
	for _, k := range Table {
		unset(t, k.Name)
	}
	cases := []struct {
		key        string
		edge, exit string
	}{
		{"HS2_PICK_LANE", "on", ""},
		{"HS2_STRIPE_AW_HEAVY", "2", ""},
		{"HS2_LB_FILL_PCT", "", "25"},
		{"HS2_LB_REC_CONFIRM", "", "1"},
		{"HS2_LB_FLAP_PEN", "", "1"},
		{"HS2_LB_JUDGE_DATA", "1", "1"},
		{"HS2_LB_EVAC", "", ""},
		{"HS2_LB_RESCUE", "1", ""},
		{"HS2_LB_RESCUE_RAW", "", ""},
		{"HS2_LB_RESCUE_CALLFREE", "1", ""},
		{"HS2_LB_CALL_RESCUE", "", ""},
		{"HS2_LB_CALL_LATE", "", ""},
		{"HS2_REFILL_WAIT_BYTES", "", ""},
		{"HS2_LB_FAST_VERDICT", "", ""},
		{"HS2_REFILL_WAIT_MS", "", ""},
		{"HS2_MCONN_STRIPE", "8", "8"},
		{"HS2_MCONN_STRIPE_JUMP_MS", "", ""},
	}
	for _, c := range cases {
		Activate(true)
		if v := Get(c.key); v != c.edge {
			t.Errorf("edge %s=%q want %q", c.key, v, c.edge)
		}
		Activate(false)
		if v := Get(c.key); v != c.exit {
			t.Errorf("exit %s=%q want %q", c.key, v, c.exit)
		}
	}
}

func TestExplicitWins(t *testing.T) {
	t.Cleanup(Deactivate)
	unset(t, "HS2_DEFAULTS")
	unset(t, "HS2_QUIET_IDLE")
	unset(t, "HS2_MCONN_STRIPE")
	Activate(false)
	os.Setenv("HS2_QUIET_IDLE", "0")
	os.Setenv("HS2_MCONN_STRIPE", "") // set but empty: off, as before
	if v := Get("HS2_QUIET_IDLE"); v != "0" {
		t.Fatalf("explicit 0 lost: %q", v)
	}
	if v := Get("HS2_MCONN_STRIPE"); v != "" {
		t.Fatalf("explicit empty lost: %q", v)
	}
	for _, a := range Applied() {
		if a == "HS2_QUIET_IDLE=1" || a == "HS2_MCONN_STRIPE=8" {
			t.Fatalf("Applied lists an explicitly set key: %v", a)
		}
	}
}

func TestLegacy(t *testing.T) {
	t.Cleanup(Deactivate)
	unset(t, "HS2_MCONN")
	for _, v := range []string{"legacy", "0", "off", "LEGACY"} {
		os.Setenv("HS2_DEFAULTS", v)
		if Activate(true) {
			t.Fatalf("HS2_DEFAULTS=%s: Activate returned true", v)
		}
		if g := Get("HS2_MCONN"); g != "" {
			t.Fatalf("HS2_DEFAULTS=%s: HS2_MCONN=%q", v, g)
		}
		if len(Applied()) != 0 {
			t.Fatalf("HS2_DEFAULTS=%s: Applied not empty", v)
		}
	}
	os.Unsetenv("HS2_DEFAULTS")
}

func TestTableUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range Table {
		if seen[k.Name] {
			t.Fatalf("duplicate %s", k.Name)
		}
		seen[k.Name] = true
	}
	if len(Table) != 41 {
		t.Fatalf("table has %d keys, want 41", len(Table))
	}
}

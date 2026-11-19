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
		{"HS2_LB_SPARE_EDGE", "", "1"},
		{"HS2_AP_SHRINK_HYST", "1", "1"},
		{"HS2_LB_REC_CONFIRM", "", "1"},
		{"HS2_LB_FLAP_PEN", "", "1"},
		{"HS2_LB_JUDGE_DATA", "1", "1"},
		{"HS2_LB_EVAC", "", ""},
		{"HS2_LB_RESCUE", "1", ""},
		{"HS2_LB_RESCUE_RAW", "", ""},
		{"HS2_LB_RESCUE_CALLFREE", "1", ""},
		{"HS2_LB_CALL_RESCUE", "", ""},
		{"HS2_LB_CALL_LATE", "1", ""},
		{"HS2_LB_RESCUE_CUT", "1", ""},
		{"HS2_LB_RESCUE_CUT_L3", "", ""},
		{"HS2_LB_RESCUE_CUT_PER_SEC", "", ""},
		{"HS2_LB_EDGE_SYNC", "1", ""},
		{"HS2_LB_SLOW_SKIP_COPY", "1", ""},
		{"HS2_REFILL_WAIT_BYTES", "1", ""},
		{"HS2_START_DIAL", "", "1"},
		{"HS2_LB_FAST_VERDICT", "", ""},
		{"HS2_B335_FASTLOWER", "", ""}, // b335: lab key, off
		{"HS2_LB_SILENT", "", "1"},
		{"HS2_LB_CUT_NONEW", "1", ""},
		{"HS2_B331_SPAREPATH", "1", "1"},
		{"HS2_CALLPATH_LOG", "0", ""},
		{"HS2_B336_CALLMOVE", "1", ""},
		{"HS2_B342_DCFAST", "1", "1"},
		{"HS2_B388_PEEREXEMPT", "1", "1"},
		{"HS2_B375_RRCOVER", "0", "0"},
		{"HS2_B375_RINGFLOOR", "0", "0"},
		{"HS2_RETIRE_KEEP_IDLE", "1", ""},
		{"HS2_MCONN_STRIPE_DL_FAIR", "1", ""},
		{"HS2_MCONN_RESCUE", "1", "1"},
		{"HS2_MCONN_RESCUE_JOINPICK", "1", ""},
		{"HS2_MCONN_RESCUE_SCALE", "1", "1"},
		{"HS2_MCONN_RESCUE_RETRY", "1", ""},
		{"HS2_MCONN_STRIPE_KEEPSTALL", "1", "1"},
		{"HS2_LM_FASTPICK", "1", ""},
		{"HS2_REFILL_WAIT_MS", "300", ""},
		{"HS2_MCONN_STRIPE", "8", "8"},
		{"HS2_MCONN_STRIPE_JUMP_MS", "", ""},
		{"HS2_MCONN_STRIPE_RESEND", "1", "1"},
		{"HS2_MCONN_STRIPE_FAST", "1", ""},
		{"HS2_MCONN_STRIPE_FAST_EARLY_MS", "200", ""},
		{"HS2_MCONN_STRIPE_FAST_SOLO", "1", ""},
		{"HS2_MCONN_STRIPE_DL_FAST", "1", ""},
		{"HS2_MCONN_STRIPE_UP_PATHQ", "1", ""},
		{"HS2_MCONN_STRIPE_UP_SPLIT", "1", ""},
		{"HS2_MCONN_STRIPE_UP_NOSHARE", "", ""},
		{"HS2_MCONN_STRIPE_SHARE_COPIES", "1", ""},
		{"HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ", "", ""},
		{"HS2_QUEUE_CTL_UP", "1", ""},
		{"HS2_QUEUE_CTL", "", "1"},
		{"HS2_QUEUE_CTL_FAST", "1", "1"},
		{"HS2_QUEUE_CTL_KEEP", "1", "1"},
		{"HS2_QUEUE_CTL_SHAPER", "mqchild", "mqchild"},
		{"HS2_LINK_CC_REFRESH", "1", "1"},
		{"HS2_QUEUE_DEBUG", "", ""},
		{"HS2_STRIPE_HEAVY_FLOOR_MB", "4", ""},
		{"HS2_STRIPE_HEAVY_FLOOR_GATE", "2", ""},
		{"HS2_STRIPE_HEAVY_FLOOR_SOFT", "", ""},
		{"HS2_STRIPE_HEAVY_FLOOR_QUS", "", ""},
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
	if len(Table) != 135 { // b375: + HS2_B375_RRCOVER, HS2_B375_RINGFLOOR; b388: + HS2_B388_PEEREXEMPT; rc3: + HS2_B342_DCFAST; b337: + HS2_B337_PATHSTALL; b331: + HS2_B331_SPAREPATH, HS2_CALLPATH_LOG, HS2_B336_CALLMOVE; b339: + HS2_B339_PATHHYST; rc2: + HS2_B339_CUTGRACE, HS2_B338_RC_GAP0_MS, HS2_B340_SKIPCUT, HS2_B340_CUTCLEAR
		t.Fatalf("table has %d keys, want 135", len(Table))
	}
}

// b315: the b307 async close is on by default, with b311's heavy sync on,
// and both b313 rescue-join keys are on.
func TestB315AsyncCloseOn(t *testing.T) {
	want := map[string]string{"HS2_B307_ASYNCCLOSE": "1", "HS2_B311_HEAVYSYNC": "1",
		"HS2_B313_RJMEMO": "1", "HS2_B313_RJFAST": "1"}
	for _, k := range Table {
		if v, ok := want[k.Name]; ok {
			if k.Value != v {
				t.Fatalf("%s default %q, want %q", k.Name, k.Value, v)
			}
			delete(want, k.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing keys: %v", want)
	}
}

// b332: the b322 exit no-watch key is on by default, on the exit only.
func TestB332ExitNoWatchOn(t *testing.T) {
	for _, k := range Table {
		if k.Name == "HS2_B322_EXITNOWATCH" {
			if k.Value != "1" || k.Side != Exit {
				t.Fatalf("HS2_B322_EXITNOWATCH = %q/%v, want \"1\"/exit", k.Value, k.Side)
			}
			return
		}
	}
	t.Fatal("HS2_B322_EXITNOWATCH missing")
}

// b334: the b327 pick list is on by default, on both sides.
func TestB334PickListOn(t *testing.T) {
	n := 0
	for _, k := range Table {
		if k.Name == "HS2_B327_PICKLIST" {
			n++
			if k.Value != "1" || k.Side != Both {
				t.Fatalf("HS2_B327_PICKLIST = %q/%v, want \"1\"/both", k.Value, k.Side)
			}
		}
	}
	if n != 1 {
		t.Fatalf("HS2_B327_PICKLIST appears %d times, want 1", n)
	}
}

// b338 (rel69p-rc1): SPAREPATH on both sides, PATHSTALL on (edge),
// CALLPATH_LOG off; rel69p-rc3: CALLMOVE on (edge), DCFAST on (both); the fast-lower lab keys stay out of the
// table (off unless set in the environment).
func TestB338Defaults(t *testing.T) {
	want := map[string]struct {
		v    string
		side Side
	}{
		"HS2_B331_SPAREPATH":  {"1", Both},
		"HS2_B337_PATHSTALL":  {"1", Edge},
		"HS2_B336_CALLMOVE":   {"1", Edge},
		"HS2_CALLPATH_LOG":    {"0", Edge},
		"HS2_B339_PATHHYST":   {"1", Edge},
		"HS2_B339_CUTGRACE":   {"0", Edge},
		"HS2_B338_RC_GAP0_MS": {"500", Both},
		"HS2_B340_SKIPCUT":    {"1", Both},
		"HS2_B340_CUTCLEAR":   {"1", Both},
		"HS2_B342_DCFAST":     {"1", Both},
		"HS2_B388_PEEREXEMPT": {"1", Both},
		"HS2_B375_RRCOVER":    {"0", Both},
		"HS2_B375_RINGFLOOR":  {"0", Both},
	}
	seen := map[string]int{}
	for _, k := range Table {
		switch k.Name {
		case "HS2_B335_FASTLOWER", "HS2_B337_FL_LOGONLY", "HS2_B337_UNCUT_DEBUG", "HS2_LB_SWAP", "HS2_B338_CM_HOLD_MS":
			t.Fatalf("%s must not be in the table", k.Name)
		}
		w, ok := want[k.Name]
		if !ok {
			continue
		}
		seen[k.Name]++
		if k.Value != w.v || k.Side != w.side {
			t.Fatalf("%s = %q/%v, want %q/%v", k.Name, k.Value, k.Side, w.v, w.side)
		}
	}
	for n := range want {
		if seen[n] != 1 {
			t.Fatalf("%s appears %d times, want 1", n, seen[n])
		}
	}
}

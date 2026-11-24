package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// b449k: the HS2_B449J_UPGSAFE totals reach status.json under
// "b449j_upgsafe" with the lab's ups.sh names, and stay out when all zero.
func TestB449kUpgSafeStatus(t *testing.T) {
	m := b449jUpgMap(engine.B449jUpgSnap{IranKept: 7, IranDone: 6, ExitKept: 3, ExitDone: 2, Reply: 1, BoundEnded: 4, ReopenFail: 5, Refused: 8})
	want := map[string]uint64{"ir_kept": 7, "ir_done": 6, "kh_kept": 3, "kh_done": 2, "kh_reply": 1, "end_bound": 4, "reopen_fail": 5, "refused": 8}
	if len(m) != len(want) {
		t.Fatalf("got %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("%s = %d, want %d (%v)", k, m[k], v, m)
		}
	}
	b, err := json.Marshal(liveStatus{B449jUpgSafe: m})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"b449j_upgsafe":{`) || !strings.Contains(string(b), `"kh_reply":1`) {
		t.Fatalf("json: %s", b)
	}
	b, _ = json.Marshal(liveStatus{})
	if strings.Contains(string(b), "b449j_upgsafe") {
		t.Fatalf("empty totals in json: %s", b)
	}
}

package engine

import (
	"fmt"
	"testing"
	"time"
)

// A long run whose address lists keep changing: the view keeps counters only
// for the paths it still shows (it used to keep one per path ever seen).
func TestLBViewForgetsGonePaths(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	view := newPathView(false)
	view.now = clk.now
	type L struct{ id int }
	for round := 0; round < 200; round++ {
		ip := fmt.Sprintf("10.0.%d.%d", round/250, round%250+1)
		mine := []LBAdvertIP{{IP: "81.0.0.1", Weight: 50}, {IP: ip, Weight: 50}}
		ls := []pathLink{
			{key: L{2 * round}, path: listenKey("81.0.0.1>116.0.0.5"), local: "81.0.0.1", remote: "116.0.0.5", serving: true},
			{key: L{2*round + 1}, path: listenKey(ip + ">116.0.0.5"), local: ip, remote: "116.0.0.5", serving: true},
		}
		rows, _ := view.build(listenDefs(mine, len(ls)), ls)
		if len(rows) != 2 {
			t.Fatalf("round %d: %d rows", round, len(rows))
		}
		clk.t = clk.t.Add(time.Second)
	}
	if n := len(view.acc); n > 2 {
		t.Fatalf("the view holds counters for %d paths, want <= 2 (it shows 2)", n)
	}
	if n := len(view.prev); n > 2 {
		t.Fatalf("the view holds %d link entries, want <= 2", n)
	}
}
